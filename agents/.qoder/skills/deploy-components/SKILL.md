---
name: deploy-components
description: Deploy, update, or upgrade Kubernetes components including sandbox-manager (manager), agent-sandbox-controller (controller, operator, sandbox-controller), or both. Handles image configuration, domain setup, and automated rollback. Use when user wants to deploy/install/update/upgrade/redeploy components, setup sandbox infrastructure, or configure deployment settings.
---

# Deploy sandbox-manager and agent-sandbox-controller

This skill handles the complete deployment workflow for `agent-sandbox-controller` and `sandbox-manager` to Kubernetes
clusters, including image configuration, domain name setup, and automatic configuration rollback.

## When to Use This Skill

Trigger this skill when the user's request involves:

**Deployment Actions:**

- Deploy, install, setup, configure, launch
- Update, upgrade, redeploy, reinstall
- 部署、安装、配置、更新、升级、重新部署

**Target Components (any of):**

- agent-sandbox-controller / sandbox-controller / controller / operator
- sandbox-manager / manager
- "组件" (components) / "服务" (services) / infrastructure

**Example Triggers:**

- "部署组件"
- "Deploy sandbox-manager"
- "Update controller with new image"
- "Install operator"
- "升级 manager"
- "Redeploy components"
- "配置并部署 sandbox-manager"

## Prerequisites

You **MUST** clarify the following information before deployment. If any information is missing, proactively ask the
user:

### Question 1: Which component(s) to deploy?

The user should specify one or both:

- **agent-sandbox-controller**
    - Synonyms: sandbox-controller, operator, controller
    - Chinese: 控制器、operator、controller

- **sandbox-manager**
    - Synonyms: manager
    - Chinese: manager、管理器

If user says "组件" (components) without specifying, ask which component(s) they want to deploy.

### Question 2: What is the container image?

Based on Question 1, confirm the image(s) for the component(s) to be deployed:

- For agent-sandbox-controller: `<registry>/<repo>/agent-sandbox-controller:<tag>`
- For sandbox-manager: `<registry>/<repo>/sandbox-manager:<tag>`
    - Additionally, when the user needs to deploy sandbox-manager, you also need to confirm whether the user wants to
      use the default envoy image or a custom image.

**Format example:** `registry.example.com/kruise/sandbox-manager:v1.0.0`

### Question 3: What is the domain name? (sandbox-manager only)

**Only required when deploying sandbox-manager.**

Ask the user:

- "Do you want to use a custom domain name for sandbox-manager?"
- "使用自定义域名还是默认的 localhost？"

Options:

- Use default: `localhost` (acceptable for testing)
- Custom domain: e.g., `example.com`, `sandbox.company.io`

### Question 4: Does the deployment already exist?

Before proceeding with deployment, you need to check if the corresponding deployment already exists. Deployments are
located in the `sandbox-system` namespace with the following names:

- for component `agent-sandbox-controller`, the deployment is "sandbox-controller-manager"
- for component `sandbox-manager`, the deployment is "sandbox-manager"

## Deployment Method

If the deployments already exists, you should directly update the existing deployment's image and skip the subsequent
deployment steps.

If the deployments do not exist, follow these steps in order:

### Step 1: Configure Domain (sandbox-manager only)

**Only execute this step if:**

- User is deploying sandbox-manager, AND
- User wants a custom domain (not localhost)

Edit these files:

1. `config/sandbox-manager/configuration_patch.yaml`
    - Replace `localhost` with user's domain name

2. `config/sandbox-manager/ingress_patch.yaml`
    - Replace ALL occurrences of `replace.with.your.domain` with user's domain name
    - **IMPORTANT:** Keep wildcards intact (e.g., `*.user-domain.com`)

### Step 2: Configure Images

Update the image in these patch files:

1. **For agent-sandbox-controller:**
    - File: `config/default/image_patch.yaml`
    - Update image to user-provided value

2. **For sandbox-manager:**
    - File: `config/sandbox-manager/image_patch.yaml`
    - Update `Sandbox manager image` to user-provided value
    - Update `Envoy proxy image` to user-provided value

### Step 3: Deploy Components

Run make commands in this order:

```bash
# If deploying both, deploy controller first, then manager
make deploy-agent-sandbox-controller  # If deploying controller
make deploy-sandbox-manager           # If deploying manager
```

**Deployment Order:**

- Both components: controller → manager
- Single component: deploy as needed

### Step 4: Rollback Configuration Files

**CRITICAL:** After deployment completes (success or failure), you **MUST** rollback all edited patch files to their
original state using git:

```bash
git checkout config/default/image_patch.yaml
git checkout config/sandbox-manager/image_patch.yaml
git checkout config/sandbox-manager/configuration_patch.yaml
git checkout config/sandbox-manager/ingress_patch.yaml
```

This ensures the repository remains clean for future deployments.

## Error Handling

- If deployment fails, still rollback configuration files
- Report clear error messages to the user
- Suggest checking kubectl logs if deployment fails

## Verification

After deployment, suggest user verify:

```bash
kubectl get pods -n <namespace>
kubectl get svc -n <namespace>
```
