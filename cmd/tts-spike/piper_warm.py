#!/usr/bin/env python3
"""Measure warm Piper synthesis with all configured voices loaded once."""

import argparse
import datetime
import importlib.metadata
import json
import os
import pathlib
import resource
import sys
import time

from piper import PiperVoice, SynthesisConfig


def parse_voice(value):
    slot, separator, model = value.partition("=")
    slot = slot.strip()
    model = model.strip()
    if not separator or not slot or not model:
        raise argparse.ArgumentTypeError("voice must use SLOT=/absolute/model.onnx")
    return slot, model


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", required=True)
    parser.add_argument("--voice", action="append", type=parse_voice, required=True)
    parser.add_argument("--runs", type=int, default=1)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--report", required=True)
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs must be positive")
    return args


def read_rss_bytes():
    try:
        for line in pathlib.Path("/proc/self/status").read_text().splitlines():
            if line.startswith("VmRSS:"):
                return int(line.split()[1]) * 1024
    except (OSError, ValueError, IndexError):
        pass
    # Linux reports KiB; this spike currently targets Debian/WSL.
    return int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss) * 1024


def expanded_text(item):
    repeat = max(1, int(item.get("repeat", 1)))
    return " ".join(item["text"].strip() for _ in range(repeat))


def safe_case_id(value):
    if not value or value in (".", "..") or "/" in value or "\\" in value:
        raise ValueError("invalid case id: {!r}".format(value))
    return value


def main():
    args = parse_args()
    corpus_path = pathlib.Path(args.corpus)
    suite = json.loads(corpus_path.read_text())
    if suite.get("version") != 1 or not suite.get("cases"):
        raise ValueError("corpus must use version 1 and contain cases")

    output_dir = pathlib.Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    mappings = dict(args.voice)
    voices = {}
    loads = []
    worker_started = time.perf_counter()
    for slot, model_value in mappings.items():
        model_path = pathlib.Path(model_value).resolve()
        if not model_path.is_file():
            raise FileNotFoundError(model_path)
        started = time.perf_counter()
        voice = PiperVoice.load(str(model_path))
        loads.append(
            {
                "voice_slot": slot,
                "model": str(model_path),
                "load_ms": round((time.perf_counter() - started) * 1000, 3),
                "rss_bytes": read_rss_bytes(),
                "sample_rate": voice.config.sample_rate,
            }
        )
        voices[slot] = voice
    worker_ready_ms = round((time.perf_counter() - worker_started) * 1000, 3)

    results = []
    synthesis_config = SynthesisConfig()
    for item in suite["cases"]:
        case_id = safe_case_id(item.get("id"))
        slot = item.get("voice_slot", "")
        if slot not in voices:
            raise ValueError("case {!r} requires voice slot {!r}".format(case_id, slot))
        voice = voices[slot]
        text = expanded_text(item)
        for run in range(1, args.runs + 1):
            output_path = output_dir / "{}-run-{:02d}.pcm".format(case_id, run)
            started = time.perf_counter()
            cpu_started = time.process_time()
            first_audio_ms = None
            audio_bytes = 0
            sample_rate = voice.config.sample_rate
            with output_path.open("wb") as output_file:
                for chunk in voice.synthesize(text, synthesis_config):
                    if first_audio_ms is None:
                        first_audio_ms = round((time.perf_counter() - started) * 1000, 3)
                    output_file.write(chunk.audio_int16_bytes)
                    audio_bytes += len(chunk.audio_int16_bytes)
                    sample_rate = chunk.sample_rate
            total_ms = round((time.perf_counter() - started) * 1000, 3)
            duration_seconds = audio_bytes / float(sample_rate * 2)
            results.append(
                {
                    "case_id": case_id,
                    "voice_slot": slot,
                    "run": run,
                    "characters": len(text),
                    "first_audio_ms": first_audio_ms,
                    "total_ms": total_ms,
                    "cpu_ms": round((time.process_time() - cpu_started) * 1000, 3),
                    "rss_bytes": read_rss_bytes(),
                    "audio_bytes": audio_bytes,
                    "audio_duration_seconds": round(duration_seconds, 3),
                    "real_time_factor": round((total_ms / 1000) / duration_seconds, 5),
                    "sample_rate": sample_rate,
                    "output": str(output_path),
                }
            )

    report = {
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "provider": "piper-warm",
        "provider_version": importlib.metadata.version("piper-tts"),
        "python": sys.version,
        "process_mode": "one_process_all_voices_loaded",
        "corpus": str(corpus_path),
        "worker_ready_ms": worker_ready_ms,
        "loads": loads,
        "peak_rss_bytes": int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss) * 1024,
        "results": results,
    }
    report_path = pathlib.Path(args.report)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
