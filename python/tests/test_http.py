"""Test HTTP/ConnectRPC projection."""

import json
import urllib.error
import urllib.request


def test_greet_http(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/Greet",
            data=json.dumps({"name": "World"}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["message"] == "Hi World"
    finally:
        server._stop_http()


def test_greet_http_via_annotated_route(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/v1/greet/World",
            method="GET",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["message"] == "Hi World"
    finally:
        server._stop_http()


def test_greet_http_different_name(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/Greet",
            data=json.dumps({"name": "Claude"}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["message"] == "Hi Claude"
    finally:
        server._stop_http()


def test_greet_http_not_found(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/DoesNotExist",
            data=b"{}",
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(req)
            assert False, "Expected 404"
        except urllib.error.HTTPError as e:
            assert e.code == 404
    finally:
        server._stop_http()


def test_greet_http_with_enum_and_tags(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/Greet",
            data=json.dumps(
                {
                    "name": "World",
                    "mood": "MOOD_HAPPY",
                    "tags": {"lang": "en", "source": "test"},
                }
            ).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["message"] == "Hi World"
        assert body["mood"] == "MOOD_HAPPY"
        assert body["tags"]["lang"] == "en"
        assert body["tags"]["source"] == "test"
    finally:
        server._stop_http()


def test_greet_group_http(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/GreetGroup",
            data=json.dumps(
                {
                    "people": [
                        {"name": "Alice", "mood": "MOOD_HAPPY"},
                        {"name": "Bob"},
                    ],
                }
            ).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["messages"] == ["Hi Alice", "Hi Bob"]
        assert body["count"] == 2
    finally:
        server._stop_http()


def test_greet_group_http_via_annotated_route(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/v1/greet:group",
            data=json.dumps(
                {
                    "people": [
                        {"name": "Alice", "mood": "MOOD_HAPPY"},
                        {"name": "Bob"},
                    ],
                }
            ).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["messages"] == ["Hi Alice", "Hi Bob"]
        assert body["count"] == 2
    finally:
        server._stop_http()


def test_greet_group_http_via_additional_binding(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/v1/group:greet",
            data=json.dumps({"people": [{"name": "Alice"}]}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body["messages"] == ["Hi Alice"]
        assert body["count"] == 1
    finally:
        server._stop_http()


def test_greet_group_http_empty(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/GreetGroup",
            data=json.dumps({"people": []}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            body = json.loads(resp.read())

        assert body.get("messages", []) == []
        assert body.get("count", 0) == 0
    finally:
        server._stop_http()


def test_greet_http_method_not_allowed(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/Greet",
            method="GET",
        )
        try:
            urllib.request.urlopen(req)
            assert False, "Expected non-200 for GET"
        except urllib.error.HTTPError as e:
            assert e.code in (405, 501)  # Method Not Allowed or Not Implemented
    finally:
        server._stop_http()


def test_greet_http_invalid_json(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/Greet",
            data=b"not valid json",
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(req)
            assert False, "Expected 400"
        except urllib.error.HTTPError as e:
            assert e.code == 400
            body = json.loads(e.read())
            assert body["error"]["code"] == "INVALID_ARGUMENT"
    finally:
        server._stop_http()


def test_greet_http_unknown_field_rejected(server):
    port = server._start_http(port=0)
    try:
        req = urllib.request.Request(
            f"http://localhost:{port}/greet.v1.GreetService/Greet",
            data=json.dumps({"name": "World", "extra": "x"}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            urllib.request.urlopen(req)
            assert False, "Expected 400"
        except urllib.error.HTTPError as e:
            assert e.code == 400
            body = json.loads(e.read())
            assert body["error"]["code"] == "INVALID_ARGUMENT"
            assert "field named \"extra\"" in body["error"]["message"]
            assert body["error"]["details"][0]["fieldViolations"][0]["field"] == "extra"
    finally:
        server._stop_http()
