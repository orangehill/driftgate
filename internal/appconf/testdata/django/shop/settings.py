from pathlib import Path

BASE_DIR = Path(__file__).resolve().parent.parent

SECRET_KEY = 'django-insecure-0000000000000000000000000000000000000000'

DEBUG = True

# Database
# https://docs.djangoproject.com/en/5.0/ref/settings/#databases

DATABASES = {
    'default': {
        'ENGINE': 'django.db.backends.postgresql',
        'NAME': 'shop',
        'USER': 'shop',
        'PASSWORD': "S3cr-django",  # local only
        'HOST': 'localhost',
        'PORT': 5432,
        'OPTIONS': {'connect_timeout': 5},
    },
    'legacy': {
        'ENGINE': 'django.db.backends.sqlite3',
        'NAME': BASE_DIR / 'db.sqlite3',
    }
}
