# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts.work-mail = {
      provider = "google-workspace";
      address = "w$\${o}\t$\"@example.com$";
      auth = "oauth2";
      oauth_client = "google";
    };
    oauth_clients.google.client_id = "a\"b\\c\${d}";
  };
}
