import json

from django.http import JsonResponse, HttpResponseBadRequest, HttpResponseNotAllowed
from django.views.decorators.csrf import csrf_exempt

from posts.models import Post


def index(request):
    return JsonResponse({"app": "django-blog", "endpoints": ["/health", "/posts"]})


def health(request):
    return JsonResponse({"status": "ok", "post_count": Post.objects.count()})


@csrf_exempt
def posts(request):
    if request.method == "GET":
        items = list(Post.objects.values("id", "title", "body", "created_at"))
        return JsonResponse({"posts": items})
    if request.method == "POST":
        try:
            payload = json.loads(request.body or b"{}")
        except json.JSONDecodeError as exc:
            return HttpResponseBadRequest(f"invalid json: {exc}")
        title = payload.get("title")
        body = payload.get("body", "")
        if not title:
            return HttpResponseBadRequest("title is required")
        post = Post.objects.create(title=title, body=body)
        return JsonResponse({"id": post.id, "title": post.title}, status=201)
    return HttpResponseNotAllowed(["GET", "POST"])
