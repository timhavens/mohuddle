# Piper 1.7.0 initial WSL spike

Date: 2026-08-22

Status: historical four-single-speaker baseline. The shared CC BY 4.0
`en_US-libritts_r-medium` result supersedes this as the deployment candidate;
see `piper-libritts-r-medium-wsl.md`. Audio quality, real-device
time-to-first-sound, network isolation, cancellation, and the Kokoro comparison
remain open; this is not a provider-selection decision.

## Host and isolation

- Debian 11 under WSL2, Linux 6.18.33.2.
- AMD Ryzen 9 5950X exposed as 8 logical CPUs.
- 11 GiB WSL memory, no swap.
- Python 3.9.2; `python3-venv` is not installed.
- `mpv` 0.32.0.
- `piper-tts` was installed with pip `--target` under `/tmp`; no user or system
  Python package and no Debian package was changed.
- The isolated Python dependency tree occupied 228 MiB. Four medium voice model
  and config pairs occupied 242 MiB.

Pinned Piper wheel:

```text
piper-tts 1.7.0
GPL-3.0-or-later
SHA-256 72adc623b977bdbbdf3d6f6bf88d66eda7cfe2ee8e7919a74a5952acb77a339e
```

The installed inference dependencies included ONNX Runtime 1.19.2 and NumPy
2.0.2. The Piper wheel includes its espeak bridge and espeak data; the full
runtime remains subject to the Piper GPL license even though it is local.

## Voices evaluated

All models were downloaded by Piper's voice downloader from the
`rhasspy/piper-voices` Hugging Face repository. These files exist only in the
temporary evaluation directory and are not distributed with MoHuddle.

| Agent slot | Piper voice | Model SHA-256 | Config SHA-256 | Model-card license note |
| --- | --- | --- | --- | --- |
| Codex | `en_US-ryan-medium` | `abf4c274862564ed647ba0d2c47f8ee7c9b717d27bdad9219100eb310db4047a` | `44034c056cb15681b2ad494307c7f3f2e4499d1253c700c711fa0a4607ffe78d` | CC BY-NC-SA 4.0 |
| Claude | `en_US-amy-medium` | `b3a6e47b57b8c7fbe6a0ce2518161a50f59a9cdd8a50835c02cb02bdd6206c18` | `95a23eb4d42909d38df73bb9ac7f45f597dbfcde2d1bf9526fdeaf5466977d77` | Refers to Mimic 3 voices; redistribution terms need clarification |
| AGY | `en_US-hfc_male-medium` | `d11e403a02bdf5a670c877b3dc56e0e1c8cece6fb30289586314dffdc0a78cb0` | `f66847424aed0bf99ecbb5d7cfde47c0a906f426a0daf7c46f305e7d21afd886` | CC BY-NC-SA 4.0 |
| Copilot | `en_US-hfc_female-medium` | `914c473788fc1fa8b63ace1cdcdb44588f4ae523d3ab37df1536616835a140b7` | `03f1fa0622b80463283592d97aca9f6e89aec345a5c56b7257723e0093c58b6c` | CC BY-NC-SA 4.0 |

These voices are suitable for private evaluation, not proposed bundled defaults.
Any eventual default voice set needs its own clear redistribution review.

## Cold CLI baseline

The generic Go harness launched a new Python/Piper process and loaded the
selected model for every case. `first audio` is the first raw PCM byte produced,
not an audible-player measurement.

The tables in this initial spike contain one run per case. Repeat runs and use
medians before making a final provider decision.

| Case | Characters | First audio | Total synthesis | Peak process RSS |
| --- | ---: | ---: | ---: | ---: |
| Short | 83 | 828 ms | 1,035 ms | 206 MiB |
| Conversational | 636 | 935 ms | 2,343 ms | 356 MiB |
| Technical prose | 247 | 823 ms | 1,352 ms | 256 MiB |
| Switch: Codex | 83 | 2,086 ms | 2,174 ms | 222 MiB |
| Switch: Claude | 77 | 990 ms | 1,035 ms | 223 MiB |
| Switch: AGY | 78 | 927 ms | 967 ms | 222 MiB |
| Switch: Copilot | 86 | 994 ms | 1,033 ms | 225 MiB |
| Long throughput | 4,373 | 1,075 ms | 9,740 ms | 350 MiB |

The long case produced about 233 seconds of audio, for an overall real-time
factor near 0.042. Throughput is comfortably faster than playback; repeated
model/process initialization dominates short-response latency.

## Warm four-voice baseline

`piper_warm.py` loaded all four models into one Python process before running the
same corpus. This models a persistent worker while deliberately avoiding any
claim that its evaluation protocol is production-ready.

- Four-model worker ready: 2,407 ms total.
- RSS immediately after loading the fourth model: 415 MiB.
- Peak RSS after the long synthesis: 1,013 MiB.

| Case | First audio | Total synthesis | Audio duration | Real-time factor |
| --- | ---: | ---: | ---: | ---: |
| Short | 47 ms | 201 ms | 4.7 s | 0.043 |
| Conversational | 198 ms | 1,473 ms | 39.5 s | 0.037 |
| Technical prose | 72 ms | 539 ms | 13.0 s | 0.041 |
| Switch: Codex | 189 ms | 189 ms | 4.5 s | 0.042 |
| Switch: Claude | 205 ms | 205 ms | 4.5 s | 0.046 |
| Switch: AGY | 207 ms | 207 ms | 4.3 s | 0.048 |
| Switch: Copilot | 192 ms | 193 ms | 4.3 s | 0.045 |
| Long throughput | 238 ms | 9,761 ms | 232.7 s | 0.042 |

The proposed 750 ms warm synthesis threshold is passed. Real audible latency is
still unmeasured and will include player startup/buffering.

## Playback probe

The generated stream is signed 16-bit, 22,050 Hz, mono PCM. `mpv` accepted it
with the raw-audio demuxer. `--really-quiet` can coexist with a narrowly restored
message level: adding `--msg-level=ao=info` produced the observable line
`AO: [null] 22050Hz mono 1ch s16` during a null-output probe. A real audio-device
run is still needed before treating that line as a time-to-first-sound proxy.

## Offline probe

The warm adapter synthesized the complete corrected corpus successfully inside
a Bubblewrap network namespace created with `--unshare-net`. This proves that
runtime network access is not required after installation. It does not prove
that no network call is attempted; that requires syscall tracing or a source
audit and remains open.

## Findings and remaining gates

- Piper's raw-output path flushes PCM as synthesis chunks are produced, so it
  avoids MP3 concatenation and temporary-file concerns.
- A persistent process materially improves first-audio latency.
- Piper voice models are one-model-per-voice. Keeping four voices warm appears
  feasible on this host, but the roughly 1 GiB observed peak is a real product
  cost.
- The corpus retains raw Markdown source alongside its normalized spoken text;
  provider quality is measured on the latter, not on reading raw URLs.
- Do not select Piper until the samples are heard, cancellation is measured,
  offline execution is verified, license/maintenance posture is accepted, and
  Kokoro is measured using the same corpus.
