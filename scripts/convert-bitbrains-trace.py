"""Converts the Bitbrains fastStorage trace into KubeRight's trace format.

Input: the GIFT-Eval redistribution of the Bitbrains GWA-T-12 fastStorage
trace (Salesforce/GiftEval, bitbrains_fast_storage/5T), an Arrow stream with
one record per VM: target = [CPU usage MHz, CPU usage % of provisioned],
past_feat_dynamic_real[0] = memory usage KB, at five-minute steps for 30 days.

Output: gzip JSON Lines, one VM per line:
  {"id", "stepSeconds", "cpuMilli": [...], "memoryBytes": [...], "cpuCapacityMilli"}
with null for missing samples. Provisioned CPU is CPU MHz divided by CPU %,
the median over samples above 1% utilization. The trace mixes hosts with
2,600 and 2,926 MHz cores, so each VM's MHz are converted to millicores at
whichever of the two makes its provisioned CPU closest to whole cores.

Usage: convert-bitbrains-trace.py <input.arrow> <output.jsonl.gz>
Requires pyarrow and numpy.
"""

from __future__ import annotations

import argparse
import gzip
import json
import math

import numpy as np
import pyarrow as pa


def read_table(path: str) -> pa.Table:
    with pa.memory_map(path, "r") as source:
        try:
            return pa.ipc.open_stream(source).read_all()
        except pa.ArrowInvalid:
            source.seek(0)
            return pa.ipc.open_file(source).read_all()


def as_list(values: np.ndarray, scale: float) -> list[float | None]:
    return [None if not math.isfinite(value) else round(value * scale, 3) for value in values]


CORE_CLOCKS_MHZ = (2600.0, 2926.0)


def core_clock(capacity_mhz: float) -> float:
    """The core clock that makes the provisioned capacity closest to whole cores."""

    def misfit(clock: float) -> float:
        cores = capacity_mhz / clock
        return abs(cores - max(1, round(cores))) / max(1, round(cores))

    return min(CORE_CLOCKS_MHZ, key=misfit)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("input")
    parser.add_argument("output")
    arguments = parser.parse_args()

    table = read_table(arguments.input)
    capacities = []
    written = 0
    with gzip.open(arguments.output, "wt", encoding="utf-8") as output:
        for row in range(table.num_rows):
            item = table.column("item_id")[row].as_py()
            target = np.array(table.column("target")[row].as_py(), dtype=float)
            dynamic = np.array(table.column("past_feat_dynamic_real")[row].as_py(), dtype=float)
            cpu_mhz, cpu_percent, memory_kb = target[0], target[1], dynamic[0]
            usable = np.isfinite(cpu_mhz) & np.isfinite(cpu_percent) & (cpu_percent > 1.0)
            if usable.sum() < 100:
                continue
            capacity_mhz = float(np.median(cpu_mhz[usable] / (cpu_percent[usable] / 100.0)))
            if not math.isfinite(capacity_mhz) or capacity_mhz <= 0:
                continue
            capacities.append(capacity_mhz)
            clock = core_clock(capacity_mhz)
            record = {
                "id": item,
                "stepSeconds": 300,
                "cpuMilli": as_list(cpu_mhz, 1000.0 / clock),
                "memoryBytes": as_list(memory_kb, 1024.0),
                "cpuCapacityMilli": round(capacity_mhz * 1000.0 / clock),
            }
            output.write(json.dumps(record, separators=(",", ":")) + "\n")
            written += 1
    values, counts = np.unique(np.round(np.array(capacities) / 100) * 100, return_counts=True)
    common = sorted(zip(counts, values), reverse=True)[:8]
    print(f"VMs read {table.num_rows}, written {written}")
    print("most common provisioned CPU (MHz, rounded to 100):", [(int(v), int(c)) for c, v in common])


if __name__ == "__main__":
    main()
