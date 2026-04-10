# django-blog: Django + SQLite + gunicorn. Single image, no compose needed.
#
# Build with:    gocracker run --dockerfile tests/examples/django-blog/Dockerfile \
#                              --context  tests/examples/django-blog --kernel ...
# Verify with:   curl -fsS :8000/health
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    DJANGO_SETTINGS_MODULE=blog.settings \
    PORT=8000

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY manage.py .
COPY blog ./blog
COPY posts ./posts

RUN mkdir -p /app/data \
    && python manage.py migrate --noinput

EXPOSE 8000
CMD ["gunicorn", "--bind", "0.0.0.0:8000", "--workers", "2", "blog.wsgi:application"]
