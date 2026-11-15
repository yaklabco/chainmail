# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts."in" = {
      provider = "hostinger";
      address = "in@example.org";
      auth = "password";
    };
  };
}
