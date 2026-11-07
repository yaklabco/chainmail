# Good blocks

Every block here parses in the shells its info string names.

```sh
chainmail search 'from:ann@example.com' --output json
```

```bash
export CHAINMAIL_SOCKET="$HOME/.local/state/chainmail/chainmail.sock"
if [[ -S "$CHAINMAIL_SOCKET" ]]; then echo up; fi
```

```fish
set -gx CHAINMAIL_SOCKET "$HOME/.local/state/chainmail/chainmail.sock"
if test -S $CHAINMAIL_SOCKET; echo up; end
```

1. A block indented inside a list item:

   ```sh title="list"
   chainmail folders
   ```

```text
this is not shell ( and is not checked
```

```go
func main() { // not checked either
```
