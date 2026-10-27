#!/usr/bin/env python3
"""离线校验 K8s 清单的「内部一致性」。

为什么需要这个脚本：
    `kubectl apply --dry-run=client` 在无集群环境下会去拉 openapi，直接失败；
    kustomize 只渲染不校验引用关系。很多低级错误因此漏到集群里才暴露，例如：
      - Deployment 的 selector 与 template.labels 不匹配（apply 直接报错）
      - Service 的 targetPort 写了个名字，但容器端口没起这个名字（流量不通）
      - envFrom 指向一个不存在的 ConfigMap（Pod 卡在 CreateContainerConfigError）
      - HPA 指向一个改名后的 Deployment（HPA 一直 <unknown>）
    这些都不需要集群就能查出来。

用法：
    python3 scripts/validate-k8s.py deployments/k8s/overlays/dev
    # 依赖：pip install pyyaml

校验通过返回 0，发现问题返回 1 并逐条打印。
"""
from __future__ import annotations

import subprocess
import sys

import yaml

WORKLOAD_KINDS = {"Deployment", "StatefulSet", "DaemonSet"}


def render(target: str) -> list[dict]:
    """用 kubectl kustomize 渲染（自动处理 base + overlay 的组合）。"""
    out = subprocess.run(
        ["kubectl", "kustomize", target],
        capture_output=True, text=True, check=False,
    )
    if out.returncode != 0:
        raise SystemExit(f"kubectl kustomize {target} failed:\n{out.stderr}")
    return [d for d in yaml.safe_load_all(out.stdout) if d]


def main() -> int:
    target = sys.argv[1] if len(sys.argv) > 1 else "deployments/k8s/base"
    docs = render(target)
    errors: list[str] = []

    # 按 namespace + kind + name 建索引
    index: dict[tuple[str, str, str], dict] = {}
    for d in docs:
        ns = (d.get("metadata") or {}).get("namespace", "")
        name = (d.get("metadata") or {}).get("name", "")
        index[(ns, d.get("kind", ""), name)] = d
        if not name:
            errors.append(f"{d.get('kind')} 缺少 metadata.name")

    configmaps = {(ns, n) for (ns, k, n) in index if k == "ConfigMap"}
    secrets = {(ns, n) for (ns, k, n) in index if k == "Secret"}
    workloads = {(ns, k, n): d for (ns, k, n), d in index.items() if k in WORKLOAD_KINDS}

    # ---- 1. Deployment selector 必须匹配 template.labels ----
    for (ns, kind, name), d in workloads.items():
        spec = d.get("spec") or {}
        sel = ((spec.get("selector") or {}).get("matchLabels") or {})
        labels = ((spec.get("template") or {}).get("metadata") or {}).get("labels") or {}
        missing = {k: v for k, v in sel.items() if labels.get(k) != v}
        if missing:
            errors.append(f"{kind}/{name}: selector 与 template.labels 不匹配 {missing}")

    # ---- 2. Service targetPort 必须是已声明的容器端口名或端口号 ----
    for (ns, kind, name), d in index.items():
        if kind != "Service":
            continue
        sel = ((d.get("spec") or {}).get("selector") or {})
        if not sel:
            continue
        # 找到匹配该 selector 的 workload
        matched = []
        for (wns, wkind, wname), wd in workloads.items():
            tpl = ((wd.get("spec") or {}).get("template") or {}).get("metadata") or {}
            tl = tpl.get("labels") or {}
            if all(tl.get(k) == v for k, v in sel.items()):
                matched.append((wkind, wname, wd))
        if not matched:
            errors.append(f"Service/{name}: selector {sel} 没有匹配任何 Pod 模板")
            continue

        for port in (d.get("spec") or {}).get("ports") or []:
            tp = port.get("targetPort")
            if isinstance(tp, int) or tp is None:
                continue
            ok = False
            for _, _, wd in matched:
                containers = ((wd.get("spec") or {}).get("template") or {}).get("spec", {}).get("containers") or []
                for c in containers:
                    for cp in c.get("ports") or []:
                        if cp.get("name") == tp:
                            ok = True
            if not ok:
                errors.append(f"Service/{name}: targetPort {tp!r} 在任何匹配的容器端口里都不存在")

    # ---- 3. envFrom / secretKeyRef 引用的对象必须存在 ----
    for (ns, kind, name), d in workloads.items():
        pod = ((d.get("spec") or {}).get("template") or {}).get("spec") or {}
        containers = (pod.get("containers") or []) + (pod.get("initContainers") or [])
        for c in containers:
            for src in c.get("envFrom") or []:
                if "configMapRef" in src:
                    ref = (ns, src["configMapRef"]["name"])
                    if ref not in configmaps:
                        errors.append(f"{kind}/{name}[{c['name']}]: envFrom 引用了不存在的 ConfigMap {ref[1]}")
                if "secretRef" in src:
                    ref = (ns, src["secretRef"]["name"])
                    if ref not in secrets:
                        errors.append(f"{kind}/{name}[{c['name']}]: envFrom 引用了不存在的 Secret {ref[1]}")
            for e in c.get("env") or []:
                vf = e.get("valueFrom") or {}
                sk = vf.get("secretKeyRef")
                if sk:
                    key = (ns, sk["name"])
                    if key not in secrets:
                        errors.append(f"{kind}/{name}[{c['name']}]: env {e['name']} 引用了不存在的 Secret {sk['name']}")
                    else:
                        s = index[(ns, "Secret", sk["name"])]
                        have = set((s.get("data") or {})) | set((s.get("stringData") or {}))
                        if sk["key"] not in have:
                            errors.append(f"{kind}/{name}[{c['name']}]: Secret {sk['name']} 缺少键 {sk['key']}")

    # ---- 4. HPA 必须指向存在的 workload ----
    for (ns, kind, name), d in index.items():
        if kind != "HorizontalPodAutoscaler":
            continue
        ref = (d.get("spec") or {}).get("scaleTargetRef") or {}
        if (ns, ref.get("kind", ""), ref.get("name", "")) not in workloads:
            errors.append(f"HPA/{name}: scaleTargetRef 指向不存在的 {ref.get('kind')}/{ref.get('name')}")
        mx = (d.get("spec") or {}).get("maxReplicas")
        mn = (d.get("spec") or {}).get("minReplicas")
        if isinstance(mx, int) and isinstance(mn, int) and mx < mn:
            errors.append(f"HPA/{name}: maxReplicas({mx}) < minReplicas({mn})")

    # ---- 5. PDB selector 必须能匹配到 Pod ----
    for (ns, kind, name), d in index.items():
        if kind != "PodDisruptionBudget":
            continue
        sel = ((d.get("spec") or {}).get("selector") or {}).get("matchLabels") or {}
        hit = False
        for (wns, wkind, wname), wd in workloads.items():
            tl = (((wd.get("spec") or {}).get("template") or {}).get("metadata") or {}).get("labels") or {}
            if all(tl.get(k) == v for k, v in sel.items()):
                hit = True
        if not hit:
            errors.append(f"PDB/{name}: selector {sel} 匹配不到任何 Pod")

    # ---- 6. 资源限制必须设置（没有 limits 会被节点 OOM 连带拖垮）----
    for (ns, kind, name), d in workloads.items():
        pod = ((d.get("spec") or {}).get("template") or {}).get("spec") or {}
        for c in pod.get("containers") or []:
            res = c.get("resources") or {}
            if "requests" not in res or "limits" not in res:
                errors.append(f"{kind}/{name}[{c['name']}]: 缺少 requests 或 limits")

    # ---- 7. 安全上下文（非 root + 禁提权）----
    for (ns, kind, name), d in workloads.items():
        pod = ((d.get("spec") or {}).get("template") or {}).get("spec") or {}
        if not (pod.get("securityContext") or {}).get("runAsNonRoot"):
            errors.append(f"{kind}/{name}: 未设置 securityContext.runAsNonRoot")
        for c in pod.get("containers") or []:
            sc = c.get("securityContext") or {}
            if sc.get("allowPrivilegeEscalation") is not False:
                errors.append(f"{kind}/{name}[{c['name']}]: 未禁用 allowPrivilegeEscalation")

    print(f"target: {target}")
    print(f"documents: {len(docs)}")
    for (ns, kind, name) in sorted(index):
        print(f"  - {kind}/{name}")

    if errors:
        print(f"\n❌ {len(errors)} 个问题:")
        for e in errors:
            print("  -", e)
        return 1
    print("\n✅ 全部检查通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
