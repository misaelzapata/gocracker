defmodule PlugApp.Application do
  use Application

  @impl true
  def start(_type, _args) do
    port = String.to_integer(System.get_env("PORT", "4000"))

    children = [
      {Plug.Cowboy, scheme: :http, plug: PlugApp.Router, options: [port: port, ip: {0, 0, 0, 0}]}
    ]

    opts = [strategy: :one_for_one, name: PlugApp.Supervisor]
    Supervisor.start_link(children, opts)
  end
end
