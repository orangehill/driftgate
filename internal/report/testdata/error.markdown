### DriftGate: ✗ 1 of 2 compared requests drifted

`run` against `baseline:local` · level `values` · mode `evolve` · 2026-10-30T10:00:00Z

| Compared | Passed | Drifted | Skipped | Observed |
|---:|---:|---:|---:|---:|
| 2 | 1 | 1 | 0 | 0 |

> kinds: key-extra 2, encoding 1, header-missing 1, header-value 1, key-missing 1, status 1, value 1

#### What changed

- [1] 1 drift in 1 request: header content-type changed value (\["application/json"\] → \["application/problem+json"\])
- [2] 1 drift in 1 request: status changed (200 → 500)
   - 1 request: 200 → 500: `api-news`

_6 more groups only in requests whose status changed, summarized there; `driftgate review` lists them._

#### Drifted requests

<details><summary><code>api-news</code> — 8 drifts, status 200 → 500</summary>

```
status 200 → 500
header-value header:content-type  ["application/json"] → ["application/problem+json"]
body differs: 5 drifts, mostly json; 1 other header differs (x-total); listed with --max-drifts N and in the JSON report
```

</details>

