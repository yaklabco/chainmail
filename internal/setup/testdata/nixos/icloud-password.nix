# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts.alice = {
      provider = "icloud";
      address = "alice@icloud.com";
      auth = "password";
    };
  };
}
