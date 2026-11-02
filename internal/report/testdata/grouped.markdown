### DriftGate: ✗ 7 of 7 compared requests drifted

`run` against `baseline:local` · level `values` · mode `evolve` · 2026-10-30T10:00:00Z

| Compared | Passed | Drifted | Skipped | Observed |
|---:|---:|---:|---:|---:|
| 7 | 0 | 7 | 0 | 0 |

#### What changed

- [1] 6 drifts in 6 requests: status changed (for example 200 → 302)
   - 3 requests: 200 → 302 to /login: `home`, `about`, `contact`
   - 2 requests: 200 → 500: `api-news`, `api-faq`
   - 1 request: 200 → 302 to http://localhost:8080/: `admin`
- [5] 4 drifts in 4 requests: header location is new
- [9] 3 drifts in 3 requests: header x-total is missing
- [10] 3 drifts in 3 requests: header content-type changed value (for example \["application/json"\] → \["application/problem+json"\])
- [16] 1 drift in 1 request: $.data\[\*\].id changed type int → string
- [17] 1 drift in 1 request: $.title changed value ("Notes" → "Notes news-0")

_11 more groups only in requests whose status changed, summarized there; `driftgate review` lists them._

#### Drifted requests

<details><summary><code>home</code> — 9 drifts, status 200 → 302, redirects to /login</summary>

```
status 200 → 302, redirects to /login
header-extra header:location   → ["/login"]
body differs: 6 html drifts; 1 other header differs (vary); listed with --max-drifts N and in the JSON report
```

</details>

<details><summary><code>about</code> — 9 drifts, status 200 → 302, redirects to /login</summary>

```
status 200 → 302, redirects to /login
header-extra header:location   → ["/login"]
body differs: 6 html drifts; 1 other header differs (vary); listed with --max-drifts N and in the JSON report
```

</details>

<details><summary><code>contact</code> — 9 drifts, status 200 → 302, redirects to /login</summary>

```
status 200 → 302, redirects to /login
header-extra header:location   → ["/login"]
body differs: 6 html drifts; 1 other header differs (vary); listed with --max-drifts N and in the JSON report
```

</details>

<details><summary><code>admin</code> — 9 drifts, status 200 → 302, redirects to http://localhost:8080/</summary>

```
status 200 → 302, redirects to http://localhost:8080/
header-extra header:location   → ["http://localhost:8080/"]
body differs: 6 html drifts; 1 other header differs (vary); listed with --max-drifts N and in the JSON report
```

</details>

<details><summary><code>api-news</code> — 8 drifts, status 200 → 500</summary>

```
status 200 → 500
header-value header:content-type  ["application/json"] → ["application/problem+json"]
body differs: 5 drifts, mostly json; 1 other header differs (x-total); listed with --max-drifts N and in the JSON report
```

</details>

<details><summary><code>api-faq</code> — 8 drifts, status 200 → 500</summary>

```
status 200 → 500
header-value header:content-type  ["application/json"] → ["application/problem+json"]
body differs: 5 drifts, mostly json; 1 other header differs (x-total); listed with --max-drifts N and in the JSON report
```

</details>

<details><summary><code>news-0</code> — 4 drifts</summary>

```
type $.data[0].id  int → string
header-missing header:x-total  ["3"] → 
header-value header:content-type  ["application/json"] → ["text/html"]
value $.title  "Notes" → "Notes news-0"
```

</details>

