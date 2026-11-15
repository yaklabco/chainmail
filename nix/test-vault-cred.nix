# A NixOS module for the VM tests that run the chainmail module. The VM has
# no TPM, so chainmail-test-cred.service makes the vault key credential at
# boot with the host key, and the module reads it from there. Production
# passes a store path instead.
{ config, pkgs, ... }:
{
  services.chainmail.vaultKeyCredential = "/var/lib/chainmail-test/vault-key.cred";

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
}
