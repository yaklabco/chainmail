# Evaluation-only check for nix/module.nix assertions: settings the module
# cannot honour must fail evaluation instead of locking clients out.
{ self, nixpkgs }:
let
  inherit (nixpkgs) lib;

  # failed returns the messages of the module's failing assertions for a
  # system that enables chainmail with extra settings.
  failed = settings:
    let
      sys = lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          self.nixosModules.default
          {
            boot.loader.grub.enable = false;
            fileSystems."/" = { device = "none"; fsType = "tmpfs"; };
            system.stateVersion = "26.05";
            services.chainmail = {
              enable = true;
              vaultKeyCredential = "/var/lib/chainmail-test/vault-key.cred";
              inherit settings;
            };
          }
        ];
      };
    in
    map (a: a.message) (lib.filter (a: !a.assertion) sys.config.assertions);

  mentions = word: msgs: lib.any (lib.hasInfix word) msgs;

  cases = {
    defaults = failed { } == [ ];
    otherClientGroup = mentions "client_group" (failed { daemon.client_group = "chainmail"; });
    socketOutsideRuntimeDir = mentions "socket" (failed { daemon.socket = "/tmp/chainmail.sock"; });
  };
  bad = lib.attrNames (lib.filterAttrs (_: ok: !ok) cases);
in
assert lib.assertMsg (bad == [ ]) "chainmail module assertion cases failed: ${toString bad}";
nixpkgs.legacyPackages.x86_64-linux.runCommand "chainmail-module-assertions" { } "touch $out"
