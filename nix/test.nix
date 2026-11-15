# NixOS VM test for nix/module.nix. The testScript is Python because that is
# the NixOS test driver's language; it is the only Python in this repository.
{ self, pkgs }:
let
  inherit (pkgs) lib;

  policySettings = {
    policy = {
      allow_expunge = false;
      send_allow = [ "*@example.com" ];
    };
  };

  policyAssert = ''
    machine.succeed("grep -q 'allow_expunge = false' /etc/chainmail/config.toml")
  '';
in
pkgs.testers.runNixOSTest {
  name = "chainmail-nixos-module";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.default ./test-vault-cred.nix ];

    services.chainmail = {
      enable = true;
      clientUsers = [ "alice" ];
      # ./test-vault-cred.nix sets vaultKeyCredential.
      # No accounts: the daemon starts with none and needs no mail servers.
      settings = policySettings;
    };

    users.users.alice.isNormalUser = true;
    users.users.mallory.isNormalUser = true;
  };

  testScript = ''
    machine.wait_for_unit("chainmail.service")
    machine.succeed("test \"$(stat -c %U /proc/$(systemctl show -p MainPID --value chainmail)/)\" = chainmail")
    machine.succeed("test \"$(stat -c '%a %U' /var/lib/chainmail)\" = '700 chainmail'")
    machine.wait_for_file("/run/chainmail/chainmail.sock")
    machine.succeed("test \"$(stat -c '%a %G' /run/chainmail/chainmail.sock)\" = '660 chainmail-clients'")
    machine.succeed("su - alice -c 'chainmail admin status'")
    machine.fail("su - mallory -c 'chainmail admin status'")
  '' + policyAssert;
}
