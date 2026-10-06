# pco completion

Print the script that completes the commands, flags and arguments of pco in a shell: bash,
zsh, fish or powershell. The script asks pco itself for the completions, so it stays right
when pco is upgraded; bash needs the package bash-completion. It changes nothing, and
anyone may run it.

## Usage

```text
pco completion <shell> [flags]
```

## Examples

```text
# Complete pco in the bash you are in
source <(pco completion bash)

# In the zsh you are in, once compinit has run
source <(pco completion zsh)

# In the fish you are in
pco completion fish | source
```

## Flags

```text
-h, --help              help for completion
    --no-descriptions   leave the descriptions out of the completions
```

## Global flags

```text
--json            print the answer of the daemon as JSON (status, routes, plan, events, claims list, guest list, diagnose, doctor, credential list, add and check, settings show and apply, route manual list and add): printed as the daemon sent it, re-indented, with control and bidirectional characters escaped; version prints the build and the schema version of the store
--socket string   unix socket of the daemon; its directory must be named pco and sit in a directory only the daemon's user can write (default "/run/pco/pco.sock")
```

## See also

- [Command reference](index.md): every command of pco, by group
