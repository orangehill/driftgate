import django.urls as urls
from items import views

urls._Resolver.url_patterns = [
    urls.URLPattern("health/", views.health),
    urls.URLResolver("api/", [urls.URLPattern("items/<int:pk>/", views.item_view(), name="item")]),
    urls.URLPattern("len/", len),
]
