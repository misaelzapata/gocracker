require "sinatra"
require "json"

set :bind, "0.0.0.0"
set :port, 4567

$counter = 0

get "/" do
  content_type :json
  { app: "sinatra-ruby", endpoints: ["/health", "/inc"] }.to_json
end

get "/health" do
  content_type :json
  { status: "ok", counter: $counter }.to_json
end

post "/inc" do
  content_type :json
  $counter += 1
  { counter: $counter }.to_json
end
