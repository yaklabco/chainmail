# One good and one bad block of each kind

```sh
chainmail accounts test work
```

```sh
echo (date)
```

```sh
for f in a b; do echo "$f"; done
```

```bash
case "$1" in a) echo a ;; esac
```

```bash
for f in a b; do echo "$f"
```

```fish
for f in a b; echo $f; end
```

```fish
if true
  echo never closed
```

A tilde fence holds a backtick fence. If the checker ended the block at the
inner fence, the closing tildes would open a new block and swallow the bad
block after it.

~~~fish
echo "a tilde fence with a nested backtick fence"
```
echo still inside the tilde block
~~~

```bash
echo "the last bad block
```
