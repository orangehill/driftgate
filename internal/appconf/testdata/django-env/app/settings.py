import os

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.mysql",
        "NAME": os.environ.get("DB_NAME", "app"),
        "USER": "app",
        "PASSWORD": os.environ["DB_PASSWORD"],
        "HOST": "127.0.0.1",
    }
}
