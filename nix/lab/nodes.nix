# lhtx lab: a local 3-node k3s cluster with Longhorn in NixOS VMs (qemu/KVM),
# for testing write actions, freezes and the agent without touching a real
# cluster. Started by the lhtx-lab script (see default.nix).
#
#   lab-1  10.77.0.11  k3s server (API on host 127.0.0.1:16443), Longhorn
#   lab-2  10.77.0.12  k3s agent
#   lab-3  10.77.0.13  k3s agent
#
# Each VM: eth0 = qemu user network (internet, port forwards), eth1 = the lab
# LAN (a qemu multicast socket shared by the three VMs on this host).
{ nixpkgs, system, lhtx, agentImage }:
let
  lib = nixpkgs.lib;
  token = "lhtx-lab-not-secret";
  ip = n: "10.77.0.1${toString n}";

  node = n: lib.nixosSystem {
    inherit system;
    modules = [
      "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
      ({ pkgs, ... }: {
        system.stateVersion = "26.11";
        networking.hostName = "lab-${toString n}";

        virtualisation = {
          memorySize = 4096;
          cores = 4;
          diskSize = 30 * 1024; # MiB, qcow2 grows on use
          graphics = false;
          forwardPorts = [
            { from = "host"; host.address = "127.0.0.1"; host.port = 2220 + n; guest.port = 22; }
          ] ++ lib.optional (n == 1)
            { from = "host"; host.address = "127.0.0.1"; host.port = 16443; guest.port = 6443; };
          qemu.networkingOptions = [
            "-netdev socket,id=lan,mcast=230.0.0.1:47700,localaddr=127.0.0.1"
            "-device virtio-net-pci,netdev=lan,mac=52:54:00:77:00:1${toString n}"
          ];
        };

        networking = {
          usePredictableInterfaceNames = false;
          firewall.enable = false;
          interfaces.eth1.ipv4.addresses = [{ address = ip n; prefixLength = 24; }];
          extraHosts = lib.concatMapStrings (i: "${ip i} lab-${toString i}\n") [ 1 2 3 ];
        };

        # ssh with the lab key from the shared directory (lhtx-lab ssh N)
        services.openssh = {
          enable = true;
          settings.PermitRootLogin = "prohibit-password";
          settings.StrictModes = false; # the key lives on a 9p share
          authorizedKeysFiles = [ "/tmp/shared/lab_key.pub" ];
        };

        # Longhorn on NixOS: iSCSI, and its tools where Longhorn looks for
        # them (it runs them in the host namespace via nsenter)
        services.openiscsi = { enable = true; name = "iqn.2026-11.lab.lhtx:lab-${toString n}"; };
        boot.kernelModules = [ "iscsi_tcp" "dm_crypt" ];
        environment.systemPackages = with pkgs; [ openiscsi nfs-utils cryptsetup util-linux kubectl lhtx ];
        systemd.tmpfiles.rules = [ "L+ /usr/local/bin - - - - /run/current-system/sw/bin/" ];
        services.multipath.enable = false;

        services.k3s = {
          enable = true;
          role = if n == 1 then "server" else "agent";
          inherit token;
          serverAddr = lib.mkIf (n != 1) "https://${ip 1}:6443";
          extraFlags = [ "--node-ip=${ip n}" "--flannel-iface=eth1" ] ++ lib.optionals (n == 1) [
            "--disable=traefik" "--disable=servicelb" "--disable=local-storage"
            "--write-kubeconfig-mode=0644" "--tls-san=127.0.0.1"
          ];
          # the agent image built from this checkout (ghcr.io/nxckdx/lhtx:lab)
          images = [ agentImage ];
          manifests = lib.mkIf (n == 1) (import ./manifests.nix);
        };

        # backup target for Longhorn: nfs://10.77.0.11:/export/backups
        services.nfs.server = lib.mkIf (n == 1) {
          enable = true;
          exports = "/export/backups 10.77.0.0/24(rw,sync,no_subtree_check,no_root_squash,fsid=1)";
        };
        systemd.tmpfiles.settings.lab-backups = lib.mkIf (n == 1) {
          "/export/backups".d = { mode = "0777"; };
        };

        # kubeconfig for the host: API forwarded to 127.0.0.1:16443
        systemd.services.lab-kubeconfig = lib.mkIf (n == 1) {
          wantedBy = [ "multi-user.target" ];
          after = [ "k3s.service" ];
          serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
          path = [ pkgs.gnused pkgs.coreutils ];
          script = ''
            while [ ! -s /etc/rancher/k3s/k3s.yaml ]; do sleep 2; done
            sed -e 's#https://127.0.0.1:6443#https://127.0.0.1:16443#' -e 's#: default$#: lhtx-lab#' \
              /etc/rancher/k3s/k3s.yaml > /tmp/shared/kubeconfig.tmp
            mv /tmp/shared/kubeconfig.tmp /tmp/shared/kubeconfig
          '';
        };
      })
    ];
  };
in
lib.genAttrs [ "lab-1" "lab-2" "lab-3" ] (name: node (lib.toInt (lib.removePrefix "lab-" name)))
