# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts.alice = {
      provider = "google-workspace";
      address = "alice@example.com";
      auth = "oauth2";
      oauth_client = "google";
    };
    oauth_clients.google.client_id = "1234.apps.googleusercontent.com";
  };
}
