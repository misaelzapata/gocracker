use actix_web::{get, post, web, App, HttpResponse, HttpServer, Responder};
use serde::{Deserialize, Serialize};
use std::sync::Mutex;

#[derive(Serialize, Deserialize, Clone)]
struct Message {
    id: usize,
    text: String,
}

struct AppState {
    messages: Mutex<Vec<Message>>,
}

#[get("/")]
async fn root() -> impl Responder {
    HttpResponse::Ok().json(serde_json::json!({
        "app": "actix-rust",
        "endpoints": ["/health", "/messages"],
    }))
}

#[get("/health")]
async fn health(state: web::Data<AppState>) -> impl Responder {
    let count = state.messages.lock().unwrap().len();
    HttpResponse::Ok().json(serde_json::json!({ "status": "ok", "count": count }))
}

#[get("/messages")]
async fn list(state: web::Data<AppState>) -> impl Responder {
    let msgs = state.messages.lock().unwrap().clone();
    HttpResponse::Ok().json(serde_json::json!({ "messages": msgs }))
}

#[derive(Deserialize)]
struct NewMessage {
    text: String,
}

#[post("/messages")]
async fn create(state: web::Data<AppState>, body: web::Json<NewMessage>) -> impl Responder {
    let mut msgs = state.messages.lock().unwrap();
    let msg = Message {
        id: msgs.len() + 1,
        text: body.text.clone(),
    };
    msgs.push(msg.clone());
    HttpResponse::Created().json(msg)
}

#[actix_web::main]
async fn main() -> std::io::Result<()> {
    let port: u16 = std::env::var("PORT")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(8080);
    let state = web::Data::new(AppState {
        messages: Mutex::new(Vec::new()),
    });
    HttpServer::new(move || {
        App::new()
            .app_data(state.clone())
            .service(root)
            .service(health)
            .service(list)
            .service(create)
    })
    .bind(("0.0.0.0", port))?
    .run()
    .await
}
