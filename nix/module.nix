# NixOS module for the chainmail daemon. The flake exposes it as
# nixosModules.default.
{ self }:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.chainmail;
  toml = pkgs.formats.toml { };
  inherit (lib) mkOption types;
in
{
  options.services.chainmail = {
    enable = lib.mkEnableOption "the chainmail policy daemon";

    package = mkOption {
      type = types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "chainmail.packages.\${pkgs.stdenv.hostPlatform.system}.default";
      description = "The chainmail package. It provides the daemon and the CLI.";
    };

    clientUsers = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [ "alice" ];
      description = ''
        Users added to the chainmail-clients group. Members may connect to
        the daemon's socket.
      '';
    };

    vaultKeyCredential = mkOption {
      type = types.either types.path types.str;
      example = lib.literalExpression "./secrets/vault-key.cred";
      description = ''
        A systemd-creds encrypted file holding the age X25519 identity that
        unlocks the vault, encrypted with the credential name vault-key. It is
        passed to the service with LoadCredentialEncrypted=. The normal
        production use is a Nix path, which is copied to the store; the
        credential is encrypted, so that is safe. An absolute path string
        points at a credential made on the host, such as one created at boot.
      '';
    };

    settings = mkOption {
      type = toml.type;
      default = { };
      description = ''
        The daemon configuration, rendered to /etc/chainmail/config.toml.
        It must not hold secrets: the file is world readable.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = lib.hasPrefix "/" (toString cfg.vaultKeyCredential);
        message = "services.chainmail.vaultKeyCredential must be a Nix path or an absolute path.";
      }
      {
        # The group, its members and the unit's SupplementaryGroups are
        # fixed, so another client_group would lock clientUsers out.
        assertion = cfg.settings.daemon.client_group == "chainmail-clients";
        message = "services.chainmail.settings.daemon.client_group must be \"chainmail-clients\"; add users with services.chainmail.clientUsers.";
      }
      {
        # ProtectSystem=strict leaves only the RuntimeDirectory writable.
        assertion = lib.hasPrefix "/run/chainmail/" cfg.settings.daemon.socket;
        message = "services.chainmail.settings.daemon.socket must be under /run/chainmail/.";
      }
    ];

    services.chainmail.settings.daemon = {
      socket = lib.mkDefault "/run/chainmail/chainmail.sock";
      client_group = lib.mkDefault "chainmail-clients";
    };

    environment.etc."chainmail/config.toml" = {
      source = toml.generate "chainmail-config.toml" cfg.settings;
      mode = "0444";
    };

    environment.systemPackages = [ cfg.package ];

    users.users.chainmail = {
      isSystemUser = true;
      group = "chainmail";
    };
    users.groups.chainmail = { };
    users.groups.chainmail-clients.members = cfg.clientUsers;

    systemd.services.chainmail = {
      description = "chainmail policy daemon";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      restartTriggers = [ config.environment.etc."chainmail/config.toml".source ];

      serviceConfig = {
        # With no --vault-key-file the daemon reads $CREDENTIALS_DIRECTORY/vault-key.
        ExecStart = "${cfg.package}/bin/chainmail daemon --config /etc/chainmail/config.toml";
        User = "chainmail";
        Group = "chainmail";
        # Lets the daemon chown its socket to the client group.
        SupplementaryGroups = [ "chainmail-clients" ];

        StateDirectory = "chainmail";
        StateDirectoryMode = "0700";
        RuntimeDirectory = "chainmail";
        # 0755, not 0750: the directory's group is chainmail, which client
        # users are not in, so 0750 stops them reaching the socket. The
        # socket's own 0660 mode and chainmail-clients group are the gate.
        RuntimeDirectoryMode = "0755";
        UMask = "0077";

        LoadCredentialEncrypted = "vault-key:${cfg.vaultKeyCredential}";

        Restart = "on-failure";
        RestartSec = "5s";

        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        NoNewPrivileges = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
      };
    };
  };
}
