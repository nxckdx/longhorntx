# lhtx-lab: start/stop the local test cluster (nodes.nix).
{ nixpkgs, system, lhtx }:
let
  pkgs = nixpkgs.legacyPackages.${system};
  # the agent image from this checkout, preloaded into k3s on every node
  agentImage = pkgs.dockerTools.buildLayeredImage {
    name = "ghcr.io/nxckdx/lhtx";
    tag = "lab";
    contents = [ ];
    config = { Entrypoint = [ "${lhtx}/bin/lhtx" ]; User = "65532:65532"; };
  };
  nodes = import ./nodes.nix { inherit nixpkgs system lhtx agentImage; };
  vm = name: nodes.${name}.config.system.build.vm;
in
{
  inherit nodes agentImage;
  script = pkgs.writeShellApplication {
    name = "lhtx-lab";
    runtimeInputs = with pkgs; [ coreutils openssh kubectl gnugrep procps lhtx ];
    text = ''
      STATE=''${LHTX_LAB_DIR:-''${XDG_STATE_HOME:-$HOME/.local/state}/lhtx-lab}
      KCFG=$STATE/shared/kubeconfig
      NODES="lab-1 lab-2 lab-3"
      declare -A RUN=(
        [lab-1]=${vm "lab-1"}/bin/run-lab-1-vm
        [lab-2]=${vm "lab-2"}/bin/run-lab-2-vm
        [lab-3]=${vm "lab-3"}/bin/run-lab-3-vm
      )

      running() { [ -f "$STATE/$1.pid" ] && kill -0 "$(cat "$STATE/$1.pid")" 2>/dev/null; }
      k() { KUBECONFIG=$KCFG kubectl --context lhtx-lab "$@"; }
      sshn() {
        local n=$1; shift
        ssh -q -i "$STATE/shared/lab_key" -p $((2220 + n)) -o StrictHostKeyChecking=no \
          -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 root@127.0.0.1 "$@"
      }

      start() {
        mkdir -p "$STATE/shared"
        [ -f "$STATE/shared/lab_key" ] || ssh-keygen -q -t ed25519 -N "" -C lhtx-lab -f "$STATE/shared/lab_key"
        for n in $NODES; do
          if running "$n"; then echo "$n: already running"; continue; fi
          mkdir -p "$STATE/tmp-$n/xchg"
          NIX_DISK_IMAGE=$STATE/$n.qcow2 SHARED_DIR=$STATE/shared TMPDIR=$STATE/tmp-$n \
            setsid "''${RUN[$n]}" > "$STATE/$n.log" 2>&1 < /dev/null &
          echo $! > "$STATE/$n.pid"
          echo "$n: started (log: lhtx-lab log ''${n#lab-})"
        done
        echo "waiting for k3s …"
        for _ in $(seq 120); do [ -s "$KCFG" ] && k get --raw /readyz >/dev/null 2>&1 && break; sleep 5; done
        echo "waiting for 3 nodes Ready …"
        for _ in $(seq 120); do
          [ "$(k get nodes --no-headers 2>/dev/null | grep -c ' Ready')" = 3 ] && break; sleep 5
        done
        k get nodes
        echo "waiting for Longhorn (first start pulls images, a few minutes) …"
        for _ in $(seq 180); do
          k -n longhorn-system get deploy longhorn-driver-deployer >/dev/null 2>&1 &&
            [ "$(k -n longhorn-system get nodes.longhorn.io --no-headers 2>/dev/null | wc -l)" = 3 ] && break
          sleep 5
        done
        k -n longhorn-system rollout status ds/longhorn-manager --timeout=15m || true
        echo
        status
      }

      stop() {
        for n in $NODES; do
          running "$n" || continue
          sshn "''${n#lab-}" poweroff 2>/dev/null || true
        done
        for n in $NODES; do
          for _ in $(seq 60); do running "$n" || break; sleep 2; done
          if running "$n"; then kill "$(cat "$STATE/$n.pid")"; echo "$n: killed"; else echo "$n: stopped"; fi
          rm -f "$STATE/$n.pid"
        done
      }

      status() {
        for n in $NODES; do
          if running "$n"; then echo "$n: running (ssh: lhtx-lab ssh ''${n#lab-})"; else echo "$n: stopped"; fi
        done
        if [ -s "$KCFG" ] && k get --raw /readyz >/dev/null 2>&1; then
          echo; k get nodes -o wide
          echo; k -n longhorn-system get volumes.longhorn.io 2>/dev/null || true
          echo; k -n lab-apps get pods,pvc 2>/dev/null || true
          echo
          echo "kubeconfig: $KCFG (context lhtx-lab)"
          echo "  lhtx-lab lhtx …      lhtx against the lab"
          echo "  lhtx-lab kubectl …   kubectl against the lab"
          echo "  eval \"\$(lhtx-lab env)\" sets KUBECONFIG in this shell"
        fi
      }

      usage() {
        cat <<EOF
      lhtx-lab – local 3-node k3s + Longhorn cluster in NixOS VMs (state: $STATE)

        start          start the VMs, wait for k3s and Longhorn
        stop           shut the VMs down (disks are kept)
        status         VMs, nodes, Longhorn volumes, test apps
        destroy        stop and delete the disks (next start is a fresh cluster)
        ssh N [cmd]    root shell on lab-N
        log N          follow the console of lab-N
        env            export KUBECONFIG=… (eval "\$(lhtx-lab env)")
        kubectl …      kubectl against the lab
        lhtx …         lhtx (this checkout) against the lab
        agent          install the lhtx agent (image built from this checkout)
      EOF
      }

      cmd=''${1:-help}; shift || true
      case $cmd in
        start) start ;;
        stop) stop ;;
        status) status ;;
        destroy)
          stop
          rm -rf "''${STATE:?}"/lab-*.qcow2 "$STATE"/tmp-* "$STATE"/shared/kubeconfig
          echo "disks deleted" ;;
        ssh) n=''${1:?node number}; shift; sshn "$n" "$@" ;;
        log) tail -f "$STATE/lab-''${1:?node number}.log" ;;
        env) echo "export KUBECONFIG=$KCFG" ;;
        kubectl) k "$@" ;;
        lhtx) exec lhtx --kubeconfig "$KCFG" --context lhtx-lab "$@" ;;
        agent)
          lhtx --kubeconfig "$KCFG" --context lhtx-lab agent manifests --tag lab |
            sed 's/imagePullPolicy: .*//; s#image: ghcr.io/nxckdx/lhtx:lab#image: ghcr.io/nxckdx/lhtx:lab\n          imagePullPolicy: Never#' |
            k apply -f -
          k -n longhorn-system rollout status deploy/lhtx-agent --timeout=2m
          lhtx --kubeconfig "$KCFG" --context lhtx-lab agent status ;;
        *) usage ;;
      esac
    '';
  };
}
