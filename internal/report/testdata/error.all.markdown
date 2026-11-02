### DriftGate: ✗ 1 of 2 compared requests drifted

`run` against `baseline:local` · level `values` · mode `evolve` · 2026-10-30T10:00:00Z

| Compared | Passed | Drifted | Skipped | Observed |
|---:|---:|---:|---:|---:|
| 2 | 1 | 1 | 0 | 0 |

#### What changed

1. 1 drift in 1 request: the same values with a different layout of whitespace
2. 1 drift in 1 request: header x-total is missing
3. 1 drift in 1 request: header content-type changed value (\["application/json"\] → \["application/problem+json"\])
4. 1 drift in 1 request: $.exception is new
5. 1 drift in 1 request: $.message is new
6. 1 drift in 1 request: $.data is missing
7. 1 drift in 1 request: status changed (200 → 500)
8. 1 drift in 1 request: $.status changed value ("ok" → "error")

#### Drifted requests

<details><summary><code>api-news</code> — 8 drifts</summary>

```
status  200 → 500
header-value header:content-type  ["application/json"] → ["application/problem+json"]
header-missing header:x-total  ["3"] → 
key-missing $.data  data → 
key-extra $.message   → message
key-extra $.exception   → exception
value $.status  "ok" → "error"
encoding $  layout → layout
```

</details>

