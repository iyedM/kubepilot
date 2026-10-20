"""Entry point: `python -m chat_api` serves the API on port 8000."""

import os

import uvicorn

from chat_api import telemetry
from chat_api.app import Settings, create_app


def main() -> None:
    tel = telemetry.configure()
    app = create_app(Settings.from_env())
    telemetry.instrument_app(app, tel)
    try:
        uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "8000")), access_log=False)
    finally:
        # Flush pending spans and metrics on SIGTERM.
        tel.shutdown()


if __name__ == "__main__":
    main()
