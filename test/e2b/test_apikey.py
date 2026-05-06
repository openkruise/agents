import os
import requests
import uuid
import pytest

def get_api_url():
    # Priority 1: E2B_API_URL (provided by CI/scripts)
    # Priority 2: Kruise-Agents specific URL based on E2B_DOMAIN
    url = os.environ.get("E2B_API_URL")
    if not url:
        domain = os.environ.get("E2B_DOMAIN", "localhost")
        url = f"http://{domain}/kruise/api"
    return url

def get_headers():
    # E2B_API_KEY is used for authentication
    api_key = os.environ.get("E2B_API_KEY", "some-api-key")
    return {
        "X-API-KEY": api_key,
        "Content-Type": "application/json"
    }


def cleanup_api_key(api_url, headers, key_id):
    """Best-effort cleanup for an API key created during tests."""
    if not key_id:
        return
    try:
        cleanup_resp = requests.delete(f"{api_url}/api-keys/{key_id}", headers=headers)
        if cleanup_resp.status_code not in (204, 404):
            print(f"Cleanup failed for key {key_id}: status={cleanup_resp.status_code}, body={cleanup_resp.text}")
    except Exception as e:
        print(f"Cleanup exception for key {key_id}: {e}")


def test_api_keys_lifecycle(sandbox_context):
    """
    Test the full lifecycle of an API key: Create, List, Delete, and List teams.
    """
    api_url = get_api_url()
    headers = get_headers()
    
    unique_suffix = str(uuid.uuid4())[:8]
    test_key_name = f"e2e-test-key-{unique_suffix}"
    created_key_id = None
    
    try:
        # 1. Create API key
        payload = {
            "name": test_key_name
        }
        create_resp = requests.post(f"{api_url}/api-keys", json=payload, headers=headers)
        assert create_resp.status_code == 201, f"Failed to create key: {create_resp.text}"
        
        try:
            created_key_data = create_resp.json()
        except ValueError:
            assert False, f"Create response is not valid JSON: {create_resp.text}"
        missing_create_fields = [field for field in ("id", "name") if field not in created_key_data]
        assert not missing_create_fields, (
            f"Create response missing required fields {missing_create_fields}. "
            f"Body: {create_resp.text}"
        )
        created_key_id = created_key_data["id"]
        assert created_key_data["name"] == test_key_name
        
        # 2. List API keys and verify existence
        list_resp = requests.get(f"{api_url}/api-keys", headers=headers)
        assert list_resp.status_code == 200
        try:
            keys_list = list_resp.json()
        except ValueError:
            assert False, f"List API keys response is not valid JSON: {list_resp.text}"
        assert isinstance(keys_list, list), f"List API keys response should be a list: {list_resp.text}"
        invalid_list_items = [
            {"index": idx, "missing_fields": [field for field in ("id", "name") if field not in key_item], "value": key_item}
            for idx, key_item in enumerate(keys_list)
            if not isinstance(key_item, dict) or any(field not in key_item for field in ("id", "name"))
        ]
        assert not invalid_list_items, (
            f"Each API key item must contain id and name. "
            f"Invalid items: {invalid_list_items[:3]} (showing 3 of {len(invalid_list_items)} invalid entries). "
            f"Body: {list_resp.text}"
        )
        assert any(k["id"] == created_key_id for k in keys_list), f"Created key {created_key_id} not found in list"
        
        # 3. List Teams
        teams_resp = requests.get(f"{api_url}/teams", headers=headers)
        assert teams_resp.status_code == 200, f"Failed to list teams: status={teams_resp.status_code}, body={teams_resp.text}"
        try:
            teams_list = teams_resp.json()
        except ValueError:
            assert False, f"List teams response is not valid JSON: {teams_resp.text}"
        assert isinstance(teams_list, list), f"List teams response should be a list: {teams_resp.text}"
        assert len(teams_list) > 0, f"Teams list is empty for admin user. body={teams_resp.text}"

        invalid_team_items = []
        for idx, t in enumerate(teams_list):
            if not isinstance(t, dict):
                invalid_team_items.append({"index": idx, "type": type(t).__name__, "value": t})
                continue
            missing_fields = []
            if "name" not in t:
                missing_fields.append("name")
            if "teamID" not in t:
                missing_fields.append("teamID")
            unexpected_fields = [field for field in ("id",) if field in t]
            if missing_fields:
                invalid_team_items.append({
                    "index": idx,
                    "missing_fields": missing_fields,
                    "value": t,
                })
            if unexpected_fields:
                invalid_team_items.append({
                    "index": idx,
                    "unexpected_fields": unexpected_fields,
                    "value": t,
                })
        assert not invalid_team_items, (
            "Each team item must contain name/teamID and must not contain id. "
            f"Invalid items: {invalid_team_items[:3]} "
            f"(showing 3 of {len(invalid_team_items)} invalid entries). "
            f"Raw teams response: {teams_resp.text}"
        )
        
        # 4. Delete API key
        delete_resp = requests.delete(f"{api_url}/api-keys/{created_key_id}", headers=headers)
        assert delete_resp.status_code == 204
        
        # 5. Verify deletion in list
        list_resp_2 = requests.get(f"{api_url}/api-keys", headers=headers)
        assert list_resp_2.status_code == 200
        try:
            keys_list_2 = list_resp_2.json()
        except ValueError:
            assert False, f"Second list API keys response is not valid JSON: {list_resp_2.text}"
        assert isinstance(keys_list_2, list), f"Second list API keys response should be a list: {list_resp_2.text}"
        assert not any(isinstance(k, dict) and k.get("id") == created_key_id for k in keys_list_2), f"Deleted key {created_key_id} still found in list"
    finally:
        cleanup_api_key(api_url, headers, created_key_id)

def test_create_api_key_invalid_namespace(sandbox_context):
    """
    Test creating an API key with an invalid namespace (non-DNS-1123).
    Verifies the fix for returning 400 instead of 500.
    """
    api_url = get_api_url()
    headers = get_headers()
    
    unique_suffix = str(uuid.uuid4())[:8]
    test_key_name = f"e2e-test-invalid-{unique_suffix}"
    
    # Kubernetes namespaces cannot have uppercase or underscores
    invalid_namespace = "INVALID_NAMESPACE"
    
    payload = {
        "name": test_key_name,
        "teamName": invalid_namespace
    }
    create_resp = requests.post(f"{api_url}/api-keys", json=payload, headers=headers)
    
    # Verify it returns 400 Bad Request
    assert create_resp.status_code == 400, f"Expected 400 for invalid namespace, got {create_resp.status_code}: {create_resp.text}"
    
    # Verify the error message contains helpful information
    error_msg = create_resp.text.lower()
    assert "invalid" in error_msg or "does not exist" in error_msg
    assert invalid_namespace.lower() in error_msg
