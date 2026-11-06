### DriftGate: ✗ 2 of 3 compared requests drifted

`run` against `baseline:local` · level `values` · mode `evolve` · 2026-10-30T10:00:00Z

| Compared | Passed | Drifted | Skipped | Observed |
|---:|---:|---:|---:|---:|
| 3 | 1 | 2 | 1 | 1 |

> 1 request without a baseline, not compared: new; new requests join the baseline with driftgate accept

> kinds: header-missing 2, header-value 2, type 2, value 2

#### What changed

1. 2 drifts in 2 requests: header x-total is missing
2. 2 drifts in 2 requests: header content-type changed value (\["application/json"\] → \["text/html"\])
3. 2 drifts in 2 requests: $.data\[\*\].id changed type int → string
4. 2 drifts in 2 requests: $.title changed value (for example "Notes" → "Notes news-0")

#### Drifted requests

<details><summary><code>news-0</code> — 4 drifts</summary>

```
type $.data[0].id  int → string
header-missing header:x-total  ["3"] → 
header-value header:content-type  ["application/json"] → ["text/html"]
value $.title  "Notes" → "Notes news-0"
```

</details>

<details><summary><code>news-1</code> — 4 drifts</summary>

```
type $.data[0].id  int → string
header-missing header:x-total  ["3"] → 
header-value header:content-type  ["application/json"] → ["text/html"]
value $.title  "Notes" → "Notes news-1"
```

</details>

<details><summary>2 skipped or observed</summary>

```
skipped  new (no baseline)
observed up (200, 2 bytes → 200, 3 bytes)
```

</details>

