from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

app = FastAPI(title="fastapi-api")

_users: list[dict] = []


class User(BaseModel):
    name: str
    email: str | None = None


@app.get("/")
def root():
    return {"app": "fastapi-api", "endpoints": ["/health", "/users"]}


@app.get("/health")
def health():
    return {"status": "ok", "count": len(_users)}


@app.get("/users")
def list_users():
    return {"users": _users}


@app.post("/users", status_code=201)
def create_user(user: User):
    record = {"id": len(_users) + 1, **user.model_dump()}
    _users.append(record)
    return record


@app.get("/users/{user_id}")
def get_user(user_id: int):
    for u in _users:
        if u["id"] == user_id:
            return u
    raise HTTPException(status_code=404, detail="not found")
