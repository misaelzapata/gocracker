defmodule PlugApp.Router do
  use Plug.Router

  plug :match
  plug :dispatch

  get "/" do
    json(conn, %{app: "plug-elixir", endpoints: ["/health"]})
  end

  get "/health" do
    json(conn, %{status: "ok"})
  end

  match _ do
    send_resp(conn, 404, "not found")
  end

  defp json(conn, body) do
    conn
    |> put_resp_content_type("application/json")
    |> send_resp(200, Jason.encode!(body))
  end
end
