from fastapi.testclient import TestClient
from service import app

client = TestClient(app)

def test_health():
    assert client.get("/healthz").status_code == 200

def test_invalid():
    assert client.post("/api/v1/forecast", json={"samples": [-1] * 168}).status_code == 422
