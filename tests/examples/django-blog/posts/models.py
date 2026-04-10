from django.db import models


class Post(models.Model):
    title = models.CharField(max_length=200)
    body = models.TextField()
    created_at = models.DateTimeField(auto_now_add=True)

    class Meta:
        app_label = "posts"
        ordering = ["-created_at"]
