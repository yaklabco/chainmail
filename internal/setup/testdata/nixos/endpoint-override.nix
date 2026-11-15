# Made by chainmail bootstrap. Import it from your NixOS configuration.
{ ... }:
{
  services.chainmail.settings = {
    accounts.alice = {
      provider = "hostinger";
      address = "alice@example.org";
      auth = "password";
      imap = { host = "127.0.0.1"; port = 1143; security = "starttls"; };
      smtp = { host = "127.0.0.1"; port = 1465; security = "tls"; tls_server_name = "mail.example.org"; };
    };
  };
}
