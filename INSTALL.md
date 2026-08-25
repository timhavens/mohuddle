# Install MoHuddle

MoHuddle coordinates separately installed AI provider command-line tools. The
download archive contains MoHuddle itself; it does not contain provider CLIs,
provider credentials, hosted-model access, speech runtimes, or speech models.

## Prerequisites

Install and authenticate at least one supported provider CLI before starting a
room:

- OpenAI Codex CLI (`codex`)
- Claude Code (`claude`)
- Google Antigravity (`agy`)
- GitHub Copilot CLI (`copilot`)

Provider accounts, authentication, quotas, policy, and billing remain the
responsibility of each user. Run `mohuddle doctor` after installation to see
which executables and optional features MoHuddle can use.

Speech is optional. Text chat continues when no speech provider, model, player,
or audio path is available. See `README.md` for the supported local speech
setup.

## Choose an archive

Stable archives and `checksums.txt` are published on the GitHub Releases page.
Development snapshots are short-lived GitHub Actions artifacts and are not
stable releases.

| Operating system | Processor | Archive target | Validation | Support |
| --- | --- | --- | --- | --- |
| Linux/WSL | x86-64 | `linux_amd64` | Native tests and smoke test | Supported |
| Linux/WSL | ARM64 | `linux_arm64` | Cross-compiled | Supported preview architecture |
| macOS | Apple silicon | `darwin_arm64` | Native tests and smoke test | Preview |
| macOS | Intel | `darwin_amd64` | Cross-compiled | Preview |
| Windows | x86-64 | `windows_amd64` | Native tests and smoke test | Preview |
| Windows | ARM64 | `windows_arm64` | Cross-compiled | Preview |

Windows builds provide the desktop TUI. The native local API remains
unavailable until the Windows named-pipe transport is implemented. macOS and
Windows archives are unsigned preview builds, so Gatekeeper or SmartScreen may
display a warning.

## Verify and install

Download the archive and `checksums.txt` from the same release. On Linux or
macOS, verify it with:

```bash
sha256sum --check --ignore-missing checksums.txt
```

Extract the archive, then place `mohuddle` somewhere on `PATH`, for example:

```bash
tar -xzf mohuddle_VERSION_linux_amd64.tar.gz
install -m 0755 mohuddle_VERSION_linux_amd64/mohuddle "$HOME/.local/bin/mohuddle"
mohuddle doctor
```

On Windows, extract the `.zip`, place `mohuddle.exe` in a directory on `PATH`,
and run:

```powershell
Get-FileHash .\mohuddle_VERSION_windows_amd64.zip -Algorithm SHA256
.\mohuddle.exe doctor
```

Compare the reported hash with `checksums.txt` before running the executable.
