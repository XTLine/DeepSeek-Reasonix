# Configuration Paths

Starting with **Reasonix v1.8.1**, Reasonix uses one user-facing home directory
for global configuration and user-owned state. CLI and desktop share this
location.

## Reasonix Home

| Platform | Reasonix home |
| --- | --- |
| macOS | `~/.reasonix` |
| Linux | `~/.reasonix` |
| Windows | `%APPDATA%\reasonix` |

Set `REASONIX_HOME` to override Reasonix home for tests, CI, or portable
installations. Normal users should not need it.

## What Lives There

| Data | Path |
| --- | --- |
| Global config | `<Reasonix home>/config.toml` |
| Global credentials | `<Reasonix home>/credentials` |
| Global slash commands | `<Reasonix home>/commands/` |
| Global skills | `<Reasonix home>/skills/` |
| Global hooks | `<Reasonix home>/settings.json` |
| Hook trust store | `<Reasonix home>/trust.json` |
| Sessions | `<Reasonix home>/sessions/` |
| Archives | `<Reasonix home>/archive/` |
| Memory | `<Reasonix home>/memory/` and `<Reasonix home>/projects/` |

Caches remain in the OS cache directory, for example
`~/Library/Caches/reasonix` on macOS, `$XDG_CACHE_HOME/reasonix` or
`~/.cache/reasonix` on Linux, and `%LOCALAPPDATA%\reasonix\cache` on Windows.
Set `REASONIX_CACHE_HOME` to override the cache root.

## Config Priority

Runtime configuration is resolved in this order:

```text
command-line flags
> project ./reasonix.toml
> global <Reasonix home>/config.toml
> compatible legacy global config
> built-in defaults
```

Writes always target the new global path:

```text
macOS/Linux: ~/.reasonix/config.toml
Windows:     %APPDATA%\reasonix\config.toml
```

## Legacy Migration

Starting with **v1.8.1**, Reasonix automatically checks legacy locations on
startup before the first config load. Migration is synchronous, one-time, and
non-destructive: old files are copied or converted to Reasonix home and left
untouched.

Legacy config sources include:

```text
~/Library/Application Support/reasonix/config.toml
~/.config/reasonix/config.toml
~/.reasonix/reasonix.toml
~/.reasonix/config.json
```

Legacy credentials and sessions are also copied into Reasonix home when the new
files do not already exist. If the new global config already exists, it wins and
legacy config files are only kept as compatibility fallbacks.
