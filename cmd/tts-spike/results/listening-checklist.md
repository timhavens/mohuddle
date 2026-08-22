# Local TTS listening review

The automated spike measures synthesis performance but cannot decide whether a
voice is pleasant, intelligible, or suitable for a particular agent. Review the
generated WAV files with real WSL audio before selecting a provider.

The current comparison set is generated outside the repository at:

```text
/tmp/mohuddle-tts-listening/
    piper/
    kokoro/
```

For each provider, listen to `short`, `conversational`, `technical`, and
`voice-switch`. Sample the beginning, middle, and end of `long`; listening to
the complete `long-throughput` response is useful for detecting fatigue and
discontinuities.

Suggested playback:

```bash
mpv /tmp/mohuddle-tts-listening/piper/short.wav
mpv /tmp/mohuddle-tts-listening/kokoro/short.wav
```

Score each provider from 1 (poor) to 5 (excellent):

| Criterion | Piper | Kokoro | Notes |
| --- | ---: | ---: | --- |
| Naturalness | | | |
| General intelligibility | | | |
| Technical terms and commands | | | |
| Four voices are easy to distinguish | | | |
| Pacing and pauses | | | |
| No clicks or gaps at sentence boundaries | | | |
| Comfortable during a long response | | | |

Also note any consistently mispronounced agent names, Markdown artifacts, or
voice/model combinations that should be excluded. These samples are prerecorded,
so they do not validate audible time-to-first-sound; that remains a separate live
player test after a provider passes this listening review.
