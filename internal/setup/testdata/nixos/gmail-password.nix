# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts.alice = {
      provider = "gmail";
      address = "alice@gmail.com";
      auth = "password";
    };
  };
}
