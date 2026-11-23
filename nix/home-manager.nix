# Home Manager module: installs lhtx and writes ~/.config/lhtx/config.yaml.
#
#   imports = [ inputs.lhtx.homeManagerModules.default ];
#   programs.lhtx = {
#     enable = true;
#     settings = {
#       defaults.refresh = "auto";
#       contexts."customer-*" = { color = "red"; refresh = "30s"; trafficBudget = "200MiB"; };
#       contexts.homelab.readOnly = false;
#     };
#   };
self:
{ config, lib, pkgs, ... }:
let
  cfg = config.programs.lhtx;
  yaml = pkgs.formats.yaml { };
in
{
  options.programs.lhtx = {
    enable = lib.mkEnableOption "lhtx, the low-traffic TUI and CLI for Longhorn";

    package = lib.mkOption {
      type = lib.types.nullOr lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.lhtx-bin;
      defaultText = lib.literalExpression "inputs.lhtx.packages.\${system}.lhtx-bin";
      description = ''
        lhtx package (prebuilt release binary by default; `.lhtx` builds from
        source). null to only write the config file.
      '';
    };

    settings = lib.mkOption {
      inherit (yaml) type;
      default = { };
      example = lib.literalExpression ''
        {
          defaults = { refresh = "auto"; readOnly = true; };
          contexts."customer-*" = { color = "red"; refresh = "30s"; trafficBudget = "200MiB"; };
        }
      '';
      description = ''
        Written to {file}`$XDG_CONFIG_HOME/lhtx/config.yaml`. Keys: `defaults`
        and `contexts.<name or glob>` with refresh, autoPollRate, readOnly,
        color, trafficBudget, overview, namespace. lhtx rejects unknown keys
        at start; check with `lhtx config`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = lib.optional (cfg.package != null) cfg.package;
    xdg.configFile."lhtx/config.yaml" = lib.mkIf (cfg.settings != { }) {
      source = yaml.generate "lhtx-config.yaml" cfg.settings;
    };
  };
}
