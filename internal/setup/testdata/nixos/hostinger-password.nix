# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts.alice = {
      provider = "hostinger";
      address = "alice@example.org";
      auth = "password";
    };
  };
}
