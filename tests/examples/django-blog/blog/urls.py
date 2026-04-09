from django.urls import path
from posts import views

urlpatterns = [
    path("", views.index),
    path("health", views.health),
    path("posts", views.posts),
]
