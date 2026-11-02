# A stand-in for django.urls, just enough for django_routes.py.
from django.urls.resolvers import RoutePattern


class URLPattern:
    def __init__(self, route, callback, name=None):
        self.pattern = RoutePattern(route)
        self.callback = callback
        self.name = name


class URLResolver:
    def __init__(self, route, patterns):
        self.pattern = RoutePattern(route)
        self.url_patterns = patterns


class _Resolver:
    url_patterns = []


def get_resolver():
    return _Resolver()
