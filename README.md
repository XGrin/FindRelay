# FindRelay

English | [简体中文](README.zh-CN.md)

FindRelay keeps Finder running and hands two of its entry points to the file manager you choose: clicking Finder in the Dock, and double-clicking a folder on the desktop. Finder still manages the desktop and file associations.

- Tested on macOS 27.0 on Apple Silicon.
- MIT license.

## What it redirects

**Dock** (`--dock`, on by default). A plain left click on the Finder icon in the Dock opens or activates the target app. Long press, drag, right click and modifier-key clicks keep their native behavior. If the target app fails to open, FindRelay opens Finder once instead.

**Desktop** (`--desktop`, off by default). Double-clicking a folder on the native desktop opens that folder in the target app. The first click passes through to Finder as usual. FindRelay redirects the second click only when it lands on the same, single selected folder icon.

These stay with Finder:

- Symlinks, aliases, app bundles and other packages, stacks, multiple selections, network and external volumes, and iCloud items.
- Finder windows and empty areas of the desktop.
- Single clicks, drags, box selection, renaming, right clicks, modifier-key clicks, and the third and later clicks of a multi-click.

The folder is sent with the standard "open" event, the same as `open -a <App> <folder>`. Some apps only add such a folder to their sidebar or recent items instead of showing it. For those apps, use `--open-command` (see below).

## Requirements

- Xcode Command Line Tools (`xcode-select --install`)
- Go 1.27.3 or later

## Build

```bash
make build              # writes dist/findrelay
./dist/findrelay version
```

## Permissions

Open **System Settings → Privacy & Security → Accessibility** and allow the program that runs `findrelay`: your terminal when you run it in the foreground, or the `findrelay` executable itself when it runs as a login service. If the event tap still cannot be created, also check **Input Monitoring**.

The desktop entry reads the Desktop folder to check what was clicked. macOS may ask for Files and Folders access. You can also allow it under **System Settings → Privacy & Security → Files and Folders**. Without that access, desktop double-clicks stay with Finder, the log shows `desktop_folder_permission_denied`, and the Dock entry keeps working.

## Usage

The flags of each run are the whole configuration. FindRelay writes no config file.

```bash
./dist/findrelay run --app "/Applications/Bloom.app"                        # Dock entry (on by default)
./dist/findrelay run --app "/Applications/Bloom.app" --desktop              # Dock and desktop entries
./dist/findrelay run --app "/Applications/Bloom.app" --dock=false --desktop  # desktop entry only
./dist/findrelay run --observe                                              # observe only, no target app needed
```

| Flag | Meaning |
| --- | --- |
| `--app <path>` | Target `.app` bundle. Required unless `--observe` is set. Finder and FindRelay itself are rejected. |
| `--dock` | Redirect Finder clicks in the Dock. On by default; `--dock=false` turns it off. |
| `--desktop` | Redirect folder double-clicks on the desktop. Off by default. |
| `--open-command <template>` | Hand folders to the target app through a shell command (see below). |
| `--dock-intercept=up\|down` | When to take over the Dock click: on release (`up`, default) or on press (`down`). |
| `--desktop-intercept=up\|down` | When to take over the second desktop click: on release (`up`, default) or on press (`down`). |
| `--observe` | Listen only and print redacted hit samples. Nothing is intercepted. |
| `--debug` | Same as `--log-level=debug`. |
| `--log-level=debug\|info\|warn\|error` | Log level. Default `warn`. |

Press `Ctrl+C` to stop; press it again to exit at once. With both entries off, FindRelay exits without creating an event tap. Run `findrelay run --help` for the full usage.

With `--debug`, each press and release prints one `gesture_event` line. Three fields explain what happened:

- `source=` tells how the decision was reached. `system` is a system-wide hit test. `off_dock` means only the Dock was asked and nothing was there. `covered` means another app's window was under the pointer. `same` and `changed` are rechecks during the gesture.
- `class=` is the redacted category, for example `dock_finder`, `desktop_icon`, `off_dock`, `covered` or `target_changed`.
- `decision=` is the outcome. `pass` lets the event through. A successful redirect is `cancel_up+activate_app` (Dock) or `cancel_desktop_up+open_directory` (desktop) with the default `up` mode, and `consume+activate_app` or `consume+open_directory` with `down`.

Logs never contain file names, paths, URLs, coordinates or the name of the app that was clicked. The last line before exit summarizes callback latency.

### Check the setup

```bash
./dist/findrelay doctor                                  # checks the target recorded in the installed service
./dist/findrelay doctor --app "/Applications/Bloom.app"  # checks the given target
```

`doctor` reports the system version, GUI session, target app, installed service and permission preflight.

### Custom open command

If the target app does not open folders from the standard "open" event, give a command template with `--open-command`. The template runs through `/bin/sh`. The folder path is passed in the `folder` environment variable, so reference it as `"$folder"`, with the quotes:

```bash
./dist/findrelay run --app "/Applications/FileOS.app" --desktop --open-command 'open -a FileOS "$folder"'
./dist/findrelay run --app "/Applications/FileOS.app" --desktop --open-command '/path/to/open_in_fileos.sh "$folder"'
./dist/findrelay run --app "/Applications/FileOS.app" --desktop --open-command 'shortcuts run "Open in FileOS" --input-path "$folder"'
```

- The template must be a single line of at most 500 characters and must reference `$folder` or `${folder}`.
- The command runs with FindRelay's own permissions. Its output is discarded and only the exit status counts. FindRelay waits up to 15 seconds.
- On failure or timeout, FindRelay logs `open_command_failed` or `open_command_timeout` and opens the folder in Finder once.
- Try the command in a terminal with a real path first. The Dock entry is not affected by this flag.

### Login service

To keep FindRelay running after the terminal closes and start it at login, put the executable in a stable place and install a user LaunchAgent:

```bash
mkdir -p ~/.local/bin && cp dist/findrelay ~/.local/bin/
~/.local/bin/findrelay service install --app "/Applications/Bloom.app" --desktop   # write the flags; does not start
~/.local/bin/findrelay service start       # start now and allow launch at login
~/.local/bin/findrelay service status      # plist, login launch, process state, whether redirection is in effect
~/.local/bin/findrelay service restart     # restart to pick up new flags
~/.local/bin/findrelay service stop        # stop and turn off launch at login; keeps the plist
~/.local/bin/findrelay service uninstall   # stop and remove the LaunchAgent
```

- `install` writes `~/Library/LaunchAgents/io.findrelay.agent.plist` and leaves the service disabled. It starts at login only after `start`. None of these commands need root.
- `install` takes the same flags as `run`. Installing again only replaces the flags. A running instance keeps its old flags until `restart`.
- The LaunchAgent records the absolute path of the executable. After moving it, install again. After replacing it with a new build, check its Accessibility permission again.
- Accessibility must be granted to the `findrelay` executable, not to the terminal. Without it the service exits with code 4 and `status` says so.
- The service does not read your shell profile. Use absolute paths in `--open-command`.
- A foreground `run` and the service share one lock per GUI session, so only one of them runs. `status` tells you when another instance holds the lock.
- The service is not restarted automatically after it exits. Run `service start` to bring it back.

The service writes redacted logs to the unified log:

```bash
log show --predicate 'subsystem == "io.findrelay.agent"' --last 10m
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Finished normally, or both entries are off |
| `2` | Usage error |
| `3` | Target app missing or invalid |
| `4` | Accessibility or Input Monitoring permission missing |
| `5` | Service management failed, for example not installed, `restart` on a stopped service, a launchctl error, or the recorded executable is missing |
| `6` | Runtime failure, or another instance already runs in this GUI session |

## Development

```bash
make check   # gofmt, go vet, go build and CGO race tests; same as CI
make fmt     # format Go sources
```

Tests that touch the real system are skipped by default:

```bash
FINDRELAY_AX_INTEGRATION=1 CGO_ENABLED=1 go test -count=1 ./internal/darwin -run Integration -v              # read-only AX hit tests; needs permission and an unlocked screen
FINDRELAY_LAUNCHCTL_INTEGRATION=1 CGO_ENABLED=1 go test -count=1 ./internal/service -run TestLaunchctlIntegration -v   # real launchctl lifecycle; skipped if the service is installed
```

## License

[MIT](LICENSE)
