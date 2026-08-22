#!/usr/bin/env python3
"""Measure warm kokoro-onnx synthesis using its shared multi-voice model."""

import argparse
import asyncio
import datetime
import importlib.metadata
import json
import pathlib
import re
import resource
import sys
import time

import onnxruntime as rt

from kokoro_onnx import Kokoro


def parse_voice(value):
    slot, separator, voice = value.partition("=")
    slot = slot.strip()
    voice = voice.strip()
    if not separator or not slot or not voice:
        raise argparse.ArgumentTypeError("voice must use SLOT=VOICE_NAME")
    return slot, voice


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--voices-file", required=True)
    parser.add_argument("--corpus", required=True)
    parser.add_argument("--voice", action="append", type=parse_voice, required=True)
    parser.add_argument("--disable-cpu-arena", action="store_true")
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
    return int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss) * 1024


def expanded_text(item):
    repeat = max(1, int(item.get("repeat", 1)))
    return " ".join(item["text"].strip() for _ in range(repeat))


def safe_case_id(value):
    if not value or value in (".", "..") or "/" in value or "\\" in value:
        raise ValueError("invalid case id: {!r}".format(value))
    return value


def sentence_segments(text):
    """Approximate MoHuddle's planned normalized sentence boundaries."""
    segments = [value.strip() for value in re.split(r"(?<=[.!?])\s+", text)]
    return [value for value in segments if value]


async def synthesize_case(kokoro, text, voice_name, output_path):
    started = time.perf_counter()
    cpu_started = time.process_time()
    first_audio_ms = None
    audio_bytes = 0
    audio_samples = 0
    sample_rate = 0
    chunks = 0
    segments = sentence_segments(text)
    with output_path.open("wb") as output_file:
        for segment in segments:
            async for samples, sample_rate in kokoro.create_stream(
                segment, voice=voice_name, speed=1.0, lang="en-us"
            ):
                if first_audio_ms is None:
                    first_audio_ms = round(
                        (time.perf_counter() - started) * 1000, 3
                    )
                # kokoro-onnx emits float32 mono samples.
                samples = samples.astype("<f4", copy=False)
                value = samples.tobytes()
                output_file.write(value)
                audio_bytes += len(value)
                audio_samples += samples.size
                chunks += 1
    total_ms = round((time.perf_counter() - started) * 1000, 3)
    duration_seconds = audio_samples / float(sample_rate)
    return {
        "first_audio_ms": first_audio_ms,
        "total_ms": total_ms,
        "cpu_ms": round((time.process_time() - cpu_started) * 1000, 3),
        "rss_bytes": read_rss_bytes(),
        "audio_bytes": audio_bytes,
        "audio_chunks": chunks,
        "segments": len(segments),
        "audio_duration_seconds": round(duration_seconds, 3),
        "real_time_factor": round((total_ms / 1000) / duration_seconds, 5),
        "sample_rate": sample_rate,
    }


async def run(args):
    corpus_path = pathlib.Path(args.corpus)
    suite = json.loads(corpus_path.read_text())
    if suite.get("version") != 1 or not suite.get("cases"):
        raise ValueError("corpus must use version 1 and contain cases")

    model_path = pathlib.Path(args.model).resolve()
    voices_path = pathlib.Path(args.voices_file).resolve()
    if not model_path.is_file() or not voices_path.is_file():
        raise FileNotFoundError("model and voices files are required")
    output_dir = pathlib.Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    started = time.perf_counter()
    if args.disable_cpu_arena:
        options = rt.SessionOptions()
        options.enable_cpu_mem_arena = False
        session = rt.InferenceSession(
            str(model_path),
            sess_options=options,
            providers=["CPUExecutionProvider"],
        )
        kokoro = Kokoro.from_session(session, str(voices_path))
    else:
        kokoro = Kokoro(str(model_path), str(voices_path))
    worker_ready_ms = round((time.perf_counter() - started) * 1000, 3)
    worker_ready_rss = read_rss_bytes()
    mappings = dict(args.voice)
    available = set(kokoro.get_voices())
    missing = sorted(set(mappings.values()) - available)
    if missing:
        raise ValueError("unknown Kokoro voices: {}".format(", ".join(missing)))

    results = []
    for item in suite["cases"]:
        case_id = safe_case_id(item.get("id"))
        slot = item.get("voice_slot", "")
        if slot not in mappings:
            raise ValueError("case {!r} requires voice slot {!r}".format(case_id, slot))
        text = expanded_text(item)
        for iteration in range(1, args.runs + 1):
            output_path = output_dir / "{}-run-{:02d}.f32le".format(case_id, iteration)
            measured = await synthesize_case(
                kokoro, text, mappings[slot], output_path
            )
            measured.update(
                {
                    "case_id": case_id,
                    "voice_slot": slot,
                    "voice": mappings[slot],
                    "run": iteration,
                    "characters": len(text),
                    "output": str(output_path),
                }
            )
            results.append(measured)

    return {
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "provider": "kokoro-onnx-warm",
        "provider_version": importlib.metadata.version("kokoro-onnx"),
        "onnxruntime_version": importlib.metadata.version("onnxruntime"),
        "python": sys.version,
        "process_mode": "one_process_shared_multi_voice_model",
        "cpu_mem_arena_enabled": not args.disable_cpu_arena,
        "model": str(model_path),
        "voices_file": str(voices_path),
        "corpus": str(corpus_path),
        "worker_ready_ms": worker_ready_ms,
        "worker_ready_rss_bytes": worker_ready_rss,
        "peak_rss_bytes": int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)
        * 1024,
        "results": results,
    }


def main():
    args = parse_args()
    value = asyncio.run(run(args))
    report_path = pathlib.Path(args.report)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(value, indent=2) + "\n")


if __name__ == "__main__":
    main()
