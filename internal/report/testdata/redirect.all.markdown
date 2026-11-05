### DriftGate: ✗ 1 of 2 compared requests drifted

`run` against `baseline:local` · level `values` · mode `evolve` · 2026-10-30T10:00:00Z

| Compared | Passed | Drifted | Skipped | Observed |
|---:|---:|---:|---:|---:|
| 2 | 1 | 1 | 0 | 0 |

#### What changed

1. 1 drift in 1 request: header location is new
2. 1 drift in 1 request: status changed (200 → 302)
3. 2 drifts in 1 request: element html\>head\>meta\[\*\] is new
4. 1 drift in 1 request: element html\>body\>a is new
5. 1 drift in 1 request: element html\>body\>h1 is missing
6. 1 drift in 1 request: header vary is missing
7. 1 drift in 1 request: text of html\>body changed ("\<h1\> \<p\>" → "Redirecting to \<a\> .")
8. 1 drift in 1 request: text of html\>head\>title changed ("Home" → "Redirecting to http://localhost:8080")

#### Drifted requests

<details><summary><code>home</code> — 9 drifts</summary>

```
status  200 → 302
header-extra header:location   → ["http://localhost:8080"]
header-missing header:vary  ["Accept-Encoding"] → 
element-extra html>head>meta[0]   → meta
element-extra html>head>meta[1]   → meta
text html>head>title  "Home" → "Redirecting to http://localhost:8080"
text html>body  "<h1> <p>" → "Redirecting to <a> ."
element-missing html>body>h1  h1 → 
element-extra html>body>a   → a
```

</details>

