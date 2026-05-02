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

def test_api_keys_lifecycle(sandbox_context):
    """
    Test the full lifecycle of an API key: Create, List, Delete, and List teams.
    """
    api_url = get_api_url()
    headers = get_headers()
    
    unique_suffix = str(uuid.uuid4())[:8]
    test_key_name = f"e2e-test-key-{unique_suffix}"
    
    # 1. Create API key
    payload = {
        "name": test_key_name
    }
    create_resp = requests.post(f"{api_url}/api-keys", json=payload, headers=headers)
    assert create_resp.status_code == 201, f"Failed to create key: {create_resp.text}"
    
    created_key_data = create_resp.json()
    key_id = created_key_data["id"]
    assert created_key_data["name"] == test_key_name
    
    # 2. List API keys and verify existence
    list_resp = requests.get(f"{api_url}/api-keys", headers=headers)
    assert list_resp.status_code == 200
    keys_list = list_resp.json()
    assert any(k["id"] == key_id for k in keys_list), f"Created key {key_id} not found in list"
    
    # 3. List Teams
    teams_resp = requests.get(f"{api_url}/teams", headers=headers)
    assert teams_resp.status_code == 200
    teams_list = teams_resp.json()
    assert len(teams_list) > 0, "Teams list should not be empty for admin user"
    # Ensure team IDs and names are present
    assert all("id" in t and "name" in t for t in teams_list)
    
    # 4. Delete API key
    delete_resp = requests.delete(f"{api_url}/api-keys/{key_id}", headers=headers)
    assert delete_resp.status_code == 204
    
    # 5. Verify deletion in list
    list_resp_2 = requests.get(f"{api_url}/api-keys", headers=headers)
    assert list_resp_2.status_code == 200
    keys_list_2 = list_resp_2.json()
    assert not any(k["id"] == key_id for k in keys_list_2), f"Deleted key {key_id} still found in list"

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
