# NixOS VM test for chainmail bootstrap on NixOS. The testScript is Python
# because that is the NixOS test driver's language.
#
# fresh has no chainmail module: bootstrap seals a vault key and writes the
# Nix files into --nix-dir. configured has the module with the account:
# bootstrap stores the password through the daemon and tests the login,
# which fails by design because the VM has no network. On both, bootstrap
# must leave /etc and the chainmail unit alone.
{ self, pkgs }:
let
  inherit (pkgs) lib;

  alice = {
    users.users.alice = {
      isNormalUser = true;
      extraGroups = [ "wheel" ];
    };
    security.sudo.wheelNeedsPassword = false;
  };

  # The root shell script from the README's NixOS vault key recovery,
  # which seals a new key in place of a lost one. The test runs it with
  # the VM's age, since `nix shell nixpkgs#age` needs the network.
  recoverScript = "\n"
    + "  age-keygen 2>/dev/null | systemd-creds encrypt --name=vault-key - chainmail-vault-key.cred.new\n"
    + "  systemd-creds decrypt --name=vault-key chainmail-vault-key.cred.new - | age-keygen -y >/dev/null\n"
    + "  mv chainmail-vault-key.cred.new chainmail-vault-key.cred";
  readmeCommand = "sudo nix shell nixpkgs#age -c bash -euo pipefail -c '" + recoverScript + "'";
in
assert lib.assertMsg (lib.hasInfix readmeCommand (builtins.readFile ../README.md))
  "the README's NixOS vault key recovery no longer matches recoverScript in nix/bootstrap-test.nix";
pkgs.testers.runNixOSTest {
  name = "chainmail-nixos-bootstrap";

  nodes.fresh = { pkgs, ... }: {
    imports = [ alice ];
    environment.systemPackages = [ self.packages.x86_64-linux.default pkgs.age ];
  };

  nodes.configured = { ... }: {
    imports = [ alice self.nixosModules.default ./test-vault-cred.nix ];

    services.chainmail = {
      enable = true;
      clientUsers = [ "alice" ];
      # ./test-vault-cred.nix sets vaultKeyCredential.
      settings.accounts.alice = {
        provider = "hostinger";
        address = "alice@example.org";
        auth = "password";
      };
    };
  };

  testScript = ''
    import json
    import shlex

    recover_script = ${builtins.toJSON recoverScript}

    def as_alice(machine, cmd):
        return machine.succeed("su - alice -c " + shlex.quote(cmd))

    def assert_no_install(machine):
        machine.succeed("test ! -e /etc/chainmail")
        machine.fail("systemctl cat chainmail.service")

    def fingerprint(machine):
        unit = machine.succeed("readlink -f \"$(systemctl show -p FragmentPath --value chainmail)\"").strip()
        assert unit.startswith("/nix/store/"), f"the chainmail unit is not from the store: {unit!r}"
        paths = "/etc/chainmail/config.toml " + unit
        return machine.succeed(f"sha256sum {paths} && stat -c '%a %U %G %n' {paths}")

    start_all()

    with subtest("fresh: bootstrap seals a vault key and writes the Nix files"):
        fresh.wait_for_unit("multi-user.target")
        bootstrap = "chainmail bootstrap alice@example.org --provider hostinger --yes --key-backup none --nix-dir /home/alice/out"
        as_alice(fresh, bootstrap)
        assert_no_install(fresh)
        for name in ["chainmail.nix", "chainmail-alice.nix", "chainmail-vault-key.cred"]:
            fresh.succeed(f"test \"$(stat -c %U /home/alice/out/{name})\" = alice")
        recipient = as_alice(fresh, "sudo systemd-creds decrypt --name=vault-key /home/alice/out/chainmail-vault-key.cred - | age-keygen -y")
        assert recipient.startswith("age1"), "the sealed vault key is not an age identity"

    with subtest("fresh: a second run leaves the credential as it was"):
        before = fresh.succeed("sha256sum /home/alice/out/chainmail-vault-key.cred")
        as_alice(fresh, bootstrap)
        assert_no_install(fresh)
        after = fresh.succeed("sha256sum /home/alice/out/chainmail-vault-key.cred")
        assert before == after, "a second bootstrap changed the vault key credential"

    with subtest("fresh: the README's key recovery seals a new key in place of the old one"):
        as_alice(fresh, "mkdir /home/alice/recover && cp /home/alice/out/chainmail-vault-key.cred /home/alice/recover/")
        old = fresh.succeed("sha256sum < /home/alice/recover/chainmail-vault-key.cred")
        as_alice(fresh, "cd /home/alice/recover && sudo bash -euo pipefail -c " + shlex.quote(recover_script))
        fresh.succeed("test ! -e /home/alice/recover/chainmail-vault-key.cred.new")
        new = fresh.succeed("sha256sum < /home/alice/recover/chainmail-vault-key.cred")
        assert old != new, "the recovery left the old credential in place"
        print(fresh.succeed("stat -c '%a %U %G' /home/alice/recover/chainmail-vault-key.cred"))
        as_alice(fresh, "cat /home/alice/recover/chainmail-vault-key.cred >/dev/null")
        recipient = as_alice(fresh, "sudo systemd-creds decrypt --name=vault-key /home/alice/recover/chainmail-vault-key.cred - | age-keygen -y")
        assert recipient.startswith("age1"), "the recovered vault key is not an age identity"

    with subtest("configured: bootstrap stores the password without touching the install"):
        configured.wait_for_unit("chainmail.service")
        configured.wait_for_file("/run/chainmail/chainmail.sock")
        pid = configured.succeed("systemctl show -p MainPID --value chainmail").strip()
        files = fingerprint(configured)
        out = as_alice(configured, "echo hunter2 | chainmail bootstrap alice@example.org --provider hostinger --yes --output json")
        result = json.loads(out)
        assert result["test"] == "failed", f"the account test should fail with no network, got {result['test']!r}"
        status = json.loads(as_alice(configured, "chainmail admin status --output json"))
        assert "accounts/alice" in status["secret_names"], f"no accounts/alice in {status['secret_names']!r}"
        after = configured.succeed("systemctl show -p MainPID --value chainmail").strip()
        assert pid == after, f"chainmail.service restarted: MainPID {pid} became {after}"
        assert fingerprint(configured) == files, "bootstrap changed the config or the unit file"
  '';
}
