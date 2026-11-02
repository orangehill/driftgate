### DriftGate: ✗ 7 of 7 compared requests drifted

`run` against `baseline:local` · level `values` · mode `evolve` · 2026-10-30T10:00:00Z

| Compared | Passed | Drifted | Skipped | Observed |
|---:|---:|---:|---:|---:|
| 7 | 0 | 7 | 0 | 0 |

#### What changed

1. 6 drifts in 6 requests: status changed (for example 200 → 302)
2. 8 drifts in 4 requests: element html\>head\>meta\[\*\] is new
3. 4 drifts in 4 requests: element html\>body\>a is new
4. 4 drifts in 4 requests: element html\>body\>h1 is missing
5. 4 drifts in 4 requests: header location is new
6. 4 drifts in 4 requests: header vary is missing
7. 4 drifts in 4 requests: text of html\>body changed ("\<h1\> \<p\>" → "Redirecting to \<a\> .")
8. 4 drifts in 4 requests: text of html\>head\>title changed (for example "Home" → "Redirecting to /login")
9. 3 drifts in 3 requests: header x-total is missing
10. 3 drifts in 3 requests: header content-type changed value (for example \["application/json"\] → \["application/problem+json"\])
11. 2 drifts in 2 requests: the same values with a different layout of whitespace
12. 2 drifts in 2 requests: $.exception is new
13. 2 drifts in 2 requests: $.message is new
14. 2 drifts in 2 requests: $.data is missing
15. 2 drifts in 2 requests: $.status changed value ("ok" → "error")
16. 1 drift in 1 request: $.data\[\*\].id changed type int → string
17. 1 drift in 1 request: $.title changed value ("Notes" → "Notes news-0")

#### Drifted requests

<details><summary><code>home</code> — 9 drifts</summary>

```
status  200 → 302
header-extra header:location   → ["/login"]
header-missing header:vary  ["Accept-Encoding"] → 
element-extra html>head>meta[0]   → meta
element-extra html>head>meta[1]   → meta
text html>head>title  "Home" → "Redirecting to /login"
text html>body  "<h1> <p>" → "Redirecting to <a> ."
element-missing html>body>h1  h1 → 
element-extra html>body>a   → a
```

</details>

<details><summary><code>about</code> — 9 drifts</summary>

```
status  200 → 302
header-extra header:location   → ["/login"]
header-missing header:vary  ["Accept-Encoding"] → 
element-extra html>head>meta[0]   → meta
element-extra html>head>meta[1]   → meta
text html>head>title  "Home" → "Redirecting to /login"
text html>body  "<h1> <p>" → "Redirecting to <a> ."
element-missing html>body>h1  h1 → 
element-extra html>body>a   → a
```

</details>

<details><summary><code>contact</code> — 9 drifts</summary>

```
status  200 → 302
header-extra header:location   → ["/login"]
header-missing header:vary  ["Accept-Encoding"] → 
element-extra html>head>meta[0]   → meta
element-extra html>head>meta[1]   → meta
text html>head>title  "Home" → "Redirecting to /login"
text html>body  "<h1> <p>" → "Redirecting to <a> ."
element-missing html>body>h1  h1 → 
element-extra html>body>a   → a
```

</details>

<details><summary><code>admin</code> — 9 drifts</summary>

```
status  200 → 302
header-extra header:location   → ["http://localhost:8080/"]
header-missing header:vary  ["Accept-Encoding"] → 
element-extra html>head>meta[0]   → meta
element-extra html>head>meta[1]   → meta
text html>head>title  "Home" → "Redirecting to http://localhost:8080/"
text html>body  "<h1> <p>" → "Redirecting to <a> ."
element-missing html>body>h1  h1 → 
element-extra html>body>a   → a
```

</details>

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

<details><summary><code>api-faq</code> — 8 drifts</summary>

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

<details><summary><code>news-0</code> — 4 drifts</summary>

```
type $.data[0].id  int → string
header-missing header:x-total  ["3"] → 
header-value header:content-type  ["application/json"] → ["text/html"]
value $.title  "Notes" → "Notes news-0"
```

</details>

