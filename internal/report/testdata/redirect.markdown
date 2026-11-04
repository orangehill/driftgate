### DriftGate: ✗ 1 of 2 compared requests drifted

`run` against `baseline:local` · level `values` · mode `evolve` · 2026-10-30T10:00:00Z

| Compared | Passed | Drifted | Skipped | Observed |
|---:|---:|---:|---:|---:|
| 2 | 1 | 1 | 0 | 0 |

#### What changed

- [1] 1 drift in 1 request: header location is new
- [2] 1 drift in 1 request: status changed (200 → 302)
   - 1 request: 200 → 302 to http://localhost:8080: `home`

_6 more groups only in requests whose status changed, summarized there; `driftgate review` lists them._

#### Drifted requests

<details><summary><code>home</code> — 9 drifts, status 200 → 302, redirects to http://localhost:8080</summary>

```
status 200 → 302, redirects to http://localhost:8080
header-extra header:location   → ["http://localhost:8080"]
body differs: 6 html drifts; 1 other header differs (vary); listed with --max-drifts N and in the JSON report
```

</details>

