# Learning & forecasting in Kilter

Kilter's decisions come from models. This page explains which models, why,
and how to plug in external ones.

## The layered model stack

| Layer | Model | Learns | Used for |
|---|---|---|---|
| Distribution | exponentially-decaying histograms (24h half-life) | per-container CPU/memory distribution | request targets (p95 CPU, peak memory) |
| Behavior | online pattern classifier | workload class: steady / diurnal / bursty / batch / growing | adaptive percentiles & headroom per class |
| Trend | least-squares trend + Holt-Winters | growth rates, demand trajectory | predictive memory sizing, OOM horizon, capacity exhaustion |
| Prior | production-trace priors (Google Borg, Alibaba) | nothing (static) | cold-start estimates, clearly labeled |
| External (optional) | any pre-trained TS foundation model | — | capacity forecasts via `--forecaster-url` |

Everything above the last row is **online and self-learning in production**:
O(1) per sample, bounded memory, no offline training pipeline, no data leaving
your cluster. A workload that changes behavior migrates classes within hours
(the detector window is 48h) and its sizing policy follows automatically.
Every decision carries its evidence (`class=bursty (cv=1.8 ac24=0.1 …)`).

## Why not embed a foundation model?

Pre-trained time-series foundation models — Amazon
[Chronos / Chronos-Bolt](https://github.com/amazon-science/chronos-forecasting),
Google TimesFM, Salesforce Moirai, Lag-Llama — are real and good at zero-shot
forecasting. They are also PyTorch transformers: hundreds of MB, Python
runtimes, GPUs for throughput. Embedding one would break what makes Kilter
deployable (single static 40MB binary, air-gap friendly, tiny attack surface)
while adding little for *distribution* questions like "what request covers p95
of this container?", which histograms answer exactly.

Where TSFMs genuinely help is **long-horizon demand forecasting** (capacity
planning, pre-scaling). So Kilter treats them as a pluggable organ, not a
heart.

## Plugging in a foundation model

Run any model behind this HTTP contract and point the brain at it:

```
POST /            {"series": [..float64], "horizon": N}
→ 200             {"forecast": [..float64]}     # length N
```

```console
$ kilter brain --forecaster-url http://chronos.models.svc:8000
```

A complete Chronos-Bolt wrapper is ~20 lines:

```python
# pip install chronos-forecasting fastapi uvicorn torch
from fastapi import FastAPI
from chronos import BaseChronosPipeline
import torch

app = FastAPI()
pipe = BaseChronosPipeline.from_pretrained("amazon/chronos-bolt-small")

@app.post("/")
def forecast(req: dict):
    ctx = torch.tensor(req["series"], dtype=torch.float32)
    q, _ = pipe.predict_quantiles(ctx.unsqueeze(0), req["horizon"], quantile_levels=[0.9])
    return {"forecast": [max(0.0, float(v)) for v in q[0, :, 0]]}
```

The brain uses the external forecaster for cluster-demand peaks
(capacity-exhaustion insights) and **falls back to its built-in Holt-Winters
models on any failure** — the model server is never in the availability path.

## The detection layer (`kilter insights`)

Predictive findings, each with evidence and time-to-impact:

- `oom-risk` — learned memory peak (or its growth trend) approaching the
  container's limit; critical when within 5%, warning with an ETA otherwise.
  Predictive sizing also bumps the recommendation *before* the OOM.
- `cpu-saturation` — sustained p95 CPU ≥ 90% of the limit (throttling).
- `capacity-exhaustion` — forecast 24h demand peak ≥ 85%/95% of schedulable
  cluster capacity.
- `growth-trend` — sustained growth detected; predictive headroom applied.

## Priors and their sources

With zero observed samples, Kilter refuses to *recommend* — but `analyze` can
still *estimate* likely waste from requests alone using utilization priors
drawn from published production-trace research: Google's Borg traces report
20–40% typical utilization; Alibaba's co-located cluster traces 40–50% CPU
with strong diurnal cycles. These estimates are always labeled as prior-based
and are replaced by measurements as soon as they exist.
