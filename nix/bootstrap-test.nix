# NixOS VM test for chainmail bootstrap on NixOS. The testScript is Python
# because that is the NixOS test driver's language.
#
# fresh has no chainmail module: bootstrap seals a vault key and writes the
# Nix files into --nix-dir. configured has the module with the account:
# bootstrap stores the password through the daemon and tests the login,
# which fails by design because the VM has no network.
{ self, pkgs }:
let
  alice = {
    users.users.alice = {
      isNormalUser = true;
      extraGroups = [ "wheel" ];
    };
    security.sudo.wheelNeedsPassword = false;
  };
in
pkgs.testers.runNixOSTest {
  name = "chainmail-nixos-bootstrap";

  nodes.fresh = { pkgs, ... }: {
    imports = [ alice ];
    environment.systemPackages = [ self.packages.x86_64-linux.default pkgs.age ];
  };

  nodes.configured = { config, pkgs, ... }: {
    imports = [ alice self.nixosModules.default ];

    services.chainmail = {
      enable = true;
      clientUsers = [ "alice" ];
      # The VM has no TPM, so chainmail-test-cred.service makes this
      # credential at boot with the host key, as in nix/test.nix.
      vaultKeyCredential = "/var/lib/chainmail-test/vault-key.cred";
      settings.accounts.alice = {
        provider = "hostinger";
        address = "alice@example.org";
        auth = "password";
      };
    };

    systemd.services.chainmail-test-cred = {
      description = "Generate the chainmail test vault key credential";
      wantedBy = [ "chainmail.service" ];
      before = [ "chainmail.service" ];
      path = [ pkgs.age config.systemd.package ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      script = ''
        if [ -e /var/lib/chainmail-test/vault-key.cred ]; then
          exit 0
        fi
        install -d -m 0700 /var/lib/chainmail-test
        age-keygen -o /var/lib/chainmail-test/key
        systemd-creds encrypt --with-key=host --name=vault-key /var/lib/chainmail-test/key /var/lib/chainmail-test/vault-key.cred
      '';
    };
  };

  testScript = ''
    import json

    def as_alice(machine, cmd):
        return machine.succeed(f"su - alice -c '{cmd}'")

    start_all()

    with subtest("fresh: bootstrap seals a vault key and writes the Nix files"):
        fresh.wait_for_unit("multi-user.target")
        bootstrap = "chainmail bootstrap alice@example.org --provider hostinger --yes --key-backup none --nix-dir /home/alice/out"
        as_alice(fresh, bootstrap)
        for name in ["chainmail.nix", "chainmail-alice.nix", "chainmail-vault-key.cred"]:
            fresh.succeed(f"test \"$(stat -c %U /home/alice/out/{name})\" = alice")
        recipient = as_alice(fresh, "sudo systemd-creds decrypt --name=vault-key /home/alice/out/chainmail-vault-key.cred - | age-keygen -y")
        assert recipient.startswith("age1"), "the sealed vault key is not an age identity"

    with subtest("fresh: a second run leaves the credential as it was"):
        before = fresh.succeed("sha256sum /home/alice/out/chainmail-vault-key.cred")
        as_alice(fresh, bootstrap)
        after = fresh.succeed("sha256sum /home/alice/out/chainmail-vault-key.cred")
        assert before == after, "a second bootstrap changed the vault key credential"

    with subtest("configured: bootstrap stores the password without restarting the daemon"):
        configured.wait_for_unit("chainmail.service")
        configured.wait_for_file("/run/chainmail/chainmail.sock")
        pid = configured.succeed("systemctl show -p MainPID --value chainmail").strip()
        out = as_alice(configured, "echo hunter2 | chainmail bootstrap alice@example.org --provider hostinger --yes --output json")
        result = json.loads(out)
        assert result["test"] == "failed", f"the account test should fail with no network, got {result['test']!r}"
        status = json.loads(as_alice(configured, "chainmail admin status --output json"))
        assert "accounts/alice" in status["secret_names"], f"no accounts/alice in {status['secret_names']!r}"
        after = configured.succeed("systemctl show -p MainPID --value chainmail").strip()
        assert pid == after, f"chainmail.service restarted: MainPID {pid} became {after}"
  '';
}
