# DriftGate: prints the URL patterns of a Django project as JSON. Read from
# standard input by `python manage.py shell`; it only reads.
import inspect
import json
import os
import re

from django.urls import URLPattern, URLResolver, get_resolver
from django.urls.resolvers import RegexPattern, RoutePattern

METHODS = ["get", "post", "put", "patch", "delete", "options"]


def regex_to_path(text):
    # Groups are matched by their parentheses: (?P<name>.+) takes the rest of
    # the path, an unnamed group is {argN}, an optional (?:...)? part is left
    # out, and a literal followed by ? (a trailing /?) is dropped.
    out = []
    i = 0
    unnamed = [0]

    def group_end(start):
        depth = 0
        k = start
        while k < len(text):
            if text[k] == "\\":
                k += 2
                continue
            if text[k] == "(":
                depth += 1
            elif text[k] == ")":
                depth -= 1
                if depth == 0:
                    return k
            k += 1
        return len(text) - 1

    while i < len(text):
        c = text[i]
        if c == "\\" and i + 1 < len(text):
            out.append(text[i + 1])
            i += 2
            continue
        if c == "(":
            j = group_end(i)
            body = text[i + 1:j]
            optional = j + 1 < len(text) and text[j + 1] == "?"
            if body.startswith("?P<"):
                name = body[3:body.index(">")]
                inner = body[body.index(">") + 1:]
                rest = "*" if inner in (".*", ".+") else ""
                if inner == ".*" or optional:
                    rest += "?"
                out.append("{" + name + rest + "}")
            elif body.startswith("?:"):
                if not optional:
                    out.append(regex_to_path(body[2:]))
            else:
                unnamed[0] += 1
                out.append("{arg%d%s}" % (unnamed[0], "?" if optional else ""))
            i = j + 1 + (1 if optional else 0)
            continue
        if c == "?":
            if out:
                out.pop()  # the character before is optional
            i += 1
            continue
        if c in "^$*+":
            i += 1
            continue
        out.append(c)
        i += 1
    return "".join(out)


def to_path(pattern):
    if isinstance(pattern, RoutePattern):
        text = str(pattern._route)
        # <path:rest> takes the rest of the path; <int:pk>, <slug> one segment.
        text = re.sub(r"<path:(\w+)>", r"{\1*}", text)
        return re.sub(r"<(?:\w+:)?(\w+)>", r"{\1}", text)
    text = pattern.regex.pattern if isinstance(pattern, RegexPattern) else str(pattern)
    return regex_to_path(text.replace(r"\Z", ""))


def methods(callback):
    actions = getattr(callback, "actions", None)  # Django REST framework view sets
    if actions:
        return sorted(m.upper() for m in actions)
    cls = getattr(callback, "view_class", None) or getattr(callback, "cls", None)
    if cls is not None:
        found = [m.upper() for m in METHODS if hasattr(cls, m) and m != "options"]
        if found:
            return found
    return ["*"]


def source(callback):
    # "path:line" of the view (its class for class-based views and view
    # sets), relative to the project; "" for code outside it (Django itself,
    # installed packages).
    target = getattr(callback, "view_class", None) or getattr(callback, "cls", None) or callback
    target = inspect.unwrap(target) if inspect.isfunction(target) else target
    try:
        path = os.path.realpath(inspect.getsourcefile(target))
        root = os.path.realpath(os.getcwd())
        if not path.startswith(root + os.sep):
            return ""
        rel = os.path.relpath(path, root).replace(os.sep, "/")
        if "/site-packages/" in "/" + rel or "/dist-packages/" in "/" + rel:
            return ""  # a virtual environment inside the project
        return "%s:%d" % (rel, inspect.getsourcelines(target)[1])
    except (TypeError, OSError):
        return ""


def walk(patterns, prefix, out):
    for p in patterns:
        if isinstance(p, URLResolver):
            walk(p.url_patterns, prefix + to_path(p.pattern), out)
        elif isinstance(p, URLPattern):
            cb = p.callback
            out.append({
                "methods": methods(cb),
                "path": "/" + (prefix + to_path(p.pattern)).lstrip("/"),
                "name": p.name or "",
                "action": getattr(cb, "__module__", "") + "." + getattr(cb, "__qualname__", getattr(cb, "__name__", "")),
                "file": source(cb),
            })


routes = []
walk(get_resolver().url_patterns, "", routes)
print("\n__DRIFTGATE_ROUTES__" + json.dumps(routes))
