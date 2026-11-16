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
            vendorHash = "sha256-0wPiYjZlIV2JOV3b+qjiidD4tNBceJqP1/UTSJP8z1A=";

            subPackages = [ "cmd/lhtx" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" "-X github.com/nxckdx/longhorntx/internal/cli.Version=${version}" ];

            # kubectl plugin: `kubectl lhtx ...`
            postInstall = ''
              ln -s $out/bin/lhtx $out/bin/kubectl-lhtx
            '';

            meta = with pkgs.lib; {
              description = "Low-traffic terminal UI and CLI for Longhorn";
              homepage = "https://github.com/nxckdx/longhorntx";
              license = licenses.mit;
              mainProgram = "lhtx";
              platforms = platforms.all;
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
