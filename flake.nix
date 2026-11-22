{
  description = "lhtx – low-traffic terminal UI and CLI for Longhorn";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      version = nixpkgs.lib.removeSuffix "\n" (builtins.readFile ./VERSION);
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        rec {
          lhtx = pkgs.buildGoModule {
            pname = "lhtx";
            inherit version;
            src = ./.;

            # Update after changing go.mod: set to pkgs.lib.fakeHash, run
            # `nix build`, copy the "got:" hash.
            vendorHash = "sha256-L5iEQc0wVaGrQzJ+zZPTlPWs2YSQD2EZ10k4yH1/1VQ=";

            subPackages = [ "cmd/lhtx" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" "-X github.com/nxckdx/longhorntx/internal/cli.Version=${version}" ];

            nativeBuildInputs = [ pkgs.installShellFiles ];

            # kubectl plugin: `kubectl lhtx ...`; shell completions
            postInstall = ''
              ln -s $out/bin/lhtx $out/bin/kubectl-lhtx
            '' + pkgs.lib.optionalString (pkgs.stdenv.buildPlatform.canExecute pkgs.stdenv.hostPlatform) ''
              installShellCompletion --cmd lhtx \
                --bash <($out/bin/lhtx completion bash) \
                --zsh <($out/bin/lhtx completion zsh) \
                --fish <($out/bin/lhtx completion fish)
            '';

            meta = with pkgs.lib; {
              description = "Low-traffic terminal UI and CLI for Longhorn";
              homepage = "https://github.com/nxckdx/longhorntx";
              license = licenses.mit;
              mainProgram = "lhtx";
              platforms = platforms.all;
            };
          };
          # Prebuilt, statically linked binary from the GitHub release
          # (no Go toolchain or module download). nix/release.json is
          # updated by the release workflow after goreleaser ran.
          lhtx-bin =
            let
              release = builtins.fromJSON (builtins.readFile ./nix/release.json);
              platform = {
                x86_64-linux = "linux_amd64";
                aarch64-linux = "linux_arm64";
                x86_64-darwin = "darwin_amd64";
                aarch64-darwin = "darwin_arm64";
              }.${system};
            in
            pkgs.stdenvNoCC.mkDerivation {
              pname = "lhtx-bin";
              inherit (release) version;
              src = pkgs.fetchurl {
                url = "https://github.com/nxckdx/longhorntx/releases/download/v${release.version}/lhtx_${release.version}_${platform}.tar.gz";
                sha256 = release.sha256.${system};
              };
              sourceRoot = ".";
              nativeBuildInputs = [ pkgs.installShellFiles ];
              installPhase = ''
                runHook preInstall
                install -Dm755 lhtx $out/bin/lhtx
                ln -s $out/bin/lhtx $out/bin/kubectl-lhtx
                runHook postInstall
              '';
              postInstall = pkgs.lib.optionalString (pkgs.stdenv.buildPlatform.canExecute pkgs.stdenv.hostPlatform) ''
                installShellCompletion --cmd lhtx \
                  --bash <($out/bin/lhtx completion bash) \
                  --zsh <($out/bin/lhtx completion zsh) \
                  --fish <($out/bin/lhtx completion fish)
              '';
              meta = lhtx.meta // {
                sourceProvenance = [ pkgs.lib.sourceTypes.binaryNativeCode ];
              };
            };

          default = lhtx;
        });

      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              gotools
              kubectl
              pv # traffic measurements, see README
            ];
          };
        });
    };
}
