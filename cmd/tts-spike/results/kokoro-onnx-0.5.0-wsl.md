# Kokoro ONNX 0.5.0 initial WSL spike

Date: 2026-08-22

Status: synthesis measurements only. Audio quality, real-device
time-to-first-sound, network isolation, and cancellation remain open; this is
not a provider-selection decision.

## Host and isolation

The host is the same Debian/WSL system recorded in the Piper result. Because the
system Python is 3.9.2 and current `kokoro-onnx` requires Python 3.10 or newer,
the spike used:

- `uv` 0.12.5 wheel, SHA-256
  `3e195ccf1ed60c8bb24a6447ce306441a4181d54b602407e09bc56e963911c15`.
- uv-managed CPython 3.12.14.
- `kokoro-onnx` 0.5.0 and ONNX Runtime 1.29.0.
- Full Kokoro v1.0 ONNX model, SHA-256
  `7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5`.
- Shared v1.0 voice bank, SHA-256
  `bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d`.

All runtime files and models were retained under one `/tmp` spike directory.
uv initially placed its managed interpreter in its default user data directory;
that exact newly-created uv directory was moved into the spike directory and a
new virtual environment was created against it. No uv data remains in the user
home from this spike.

The isolated Python environment occupied 193 MiB, the managed interpreter 108
MiB, and the full model plus shared voices 338 MiB.

## License surface

- `kokoro-onnx` declares MIT in its repository.
- Kokoro model weights declare Apache 2.0.
- ONNX Runtime declares MIT.
- The installed `phonemizer-fork` package declares GPL-3.0 and the runtime also
  installs `espeakng-loader`; Kokoro is therefore not automatically a
  permissive-only bundle merely because its wrapper and weights are permissive.

The spike uses a user-installed runtime posture. Bundling still requires a full
dependency and license decision.

## Voices evaluated

One model and one shared voice bank supplied all four voices without model
reloads:

| Agent slot | Kokoro voice |
| --- | --- |
| Codex | `am_adam` |
| Claude | `af_sarah` |
| AGY | `am_michael` |
| Copilot | `af_nova` |

## Full-model warm baseline

The initial call to `kokoro-onnx.create_stream` used the library's own batching.
It grouped up to roughly 510 phonemes, causing first audio to take approximately
7–8 seconds for medium and long cases. The production design already calls for
safe sentence segmentation, so the recorded comparison below splits normalized
text into sentences before invoking the provider.

The tables in this initial spike contain one run per case. Repeat runs and use
medians before making a final provider decision.

- Shared-model worker ready: 977 ms.
- Worker-ready RSS: 491 MiB.
- Peak RSS after the long synthesis: 949 MiB.

| Case | First audio | Total synthesis | Audio duration | Real-time factor |
| --- | ---: | ---: | ---: | ---: |
| Short | 914 ms | 2,430 ms | 4.9 s | 0.491 |
| Conversational | 1,015 ms | 10,153 ms | 36.5 s | 0.278 |
| Technical prose | 505 ms | 5,675 ms | 16.2 s | 0.350 |
| Switch: Codex | 1,347 ms | 1,347 ms | 5.0 s | 0.268 |
| Switch: Claude | 1,176 ms | 1,176 ms | 4.2 s | 0.283 |
| Switch: AGY | 1,485 ms | 1,485 ms | 5.9 s | 0.251 |
| Switch: Copilot | 1,206 ms | 1,206 ms | 5.2 s | 0.234 |
| Long throughput | 1,619 ms | 72,878 ms | 254.1 s | 0.287 |

Kokoro stays faster than playback overall, and one shared model makes voice
switching structurally attractive. First audio depends heavily on first-sentence
length, however, and all cases except the technical sample miss the proposed
750 ms target in this run.

## INT8 CPU variant

The official 92 MB INT8 model was downloaded with SHA-256
`6e742170d309016e5891a994e1ce1559c702a2ccd0075e67ef7157974f6406cb`.
It was not faster on this AMD/ONNX Runtime combination. The identical segmented
run was stopped after nearly five minutes while still processing the long case,
versus about 73 seconds for the full model. The partial run produced no complete
report and is rejected as the deployment variant for this host.

This Ryzen 9 5950X exposes AVX2/FMA but not AVX-VNNI or AVX512-VNNI. The lack of
VNNI is a plausible explanation for the INT8 regression, but the spike did not
profile individual kernels; graph quantize/dequantize overhead or operator
selection may also contribute. Treat this as a hardware-specific hypothesis and
retest only with profiling or on a VNNI-capable host.

## CPU arena disabled

Constructing the ONNX session with `enable_cpu_mem_arena=False` did not reduce
Kokoro memory on this sentence-segmented workload:

| Measurement | Default arena | Arena disabled |
| --- | ---: | ---: |
| Worker-ready RSS | 491 MiB | 479 MiB |
| Peak RSS | 949 MiB | 1,457 MiB |
| Final RSS after long case | 950 MiB | 1,209 MiB |
| Long total synthesis | 72.9 s | 75.8 s |
| Long real-time factor | 0.287 | 0.298 |

The no-arena run likely incurred repeated allocator work across 36 sentence
calls. Whatever the mechanism, the measured result rejects arena disabling for
this Kokoro adapter on this host.

## CPU contention

The default long run consumed 553 CPU-seconds for 254 seconds of speech, or
about 2.18 CPU-seconds per spoken second. The shared-model Piper candidate used
about 0.29 CPU-seconds per spoken second: Kokoro consumed approximately 7.5
times more CPU for this comparison. Because WSL exposes eight logical CPUs,
voice quality must be clearly better to justify that sustained contention during
multi-agent work.

## Offline probe

The warm adapter synthesized the complete corrected corpus successfully inside
a Bubblewrap network namespace created with `--unshare-net`. This proves that
runtime network access is not required after installation. It does not prove
that no network call is attempted; that requires syscall tracing or a source
audit and remains open.

## Findings and remaining gates

- Kokoro's shared multi-voice model is operationally simpler than four separate
  Piper model sessions.
- Full-model memory is similar to the observed four-voice Piper peak, but Kokoro
  uses substantially more CPU time and has higher first-audio latency here.
- Sentence segmentation is required; the library's default streaming batches
  are too large for conversational latency.
- `create_stream` creates an internal background task without a cancellation/
  `finally` path. Cancelling MoHuddle's consumer cannot yet be assumed to cancel
  in-flight ONNX work; production integration needs a wrapper/process strategy
  with a measured bound.
- Do not select or reject Kokoro until its samples are heard alongside Piper and
  the remaining cancellation/offline/player gates are run.
