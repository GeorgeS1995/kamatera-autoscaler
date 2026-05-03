# kamatera-autoscaler

Кластерный автоскейлер K8s-нод для [Kamatera Cloud](https://www.kamatera.com/).
Следит за `Pending`-подами, которые не могут быть запланированы из-за нехватки
CPU/памяти, провижинит виртуалки через REST API Kamatera и дренирует idle-ноды
в пулах, где количество нод выше `min_nodes`.

> Read this in [English](README.md).

Автоскейлер сознательно **не привязан к дистрибутиву K8s** — он не знает про
k3s, RKE2, kubeadm, Talos и пр. Присоединение новой VM к кластеру — задача
cloud-init-template-а, который пишете вы.

---

## Архитектура

```
   ┌────────────────────────────────────────────────────────────┐
   │ kamatera-autoscaler (Deployment, kube-system)              │
   │                                                            │
   │   scaleUp tick (30s)                                       │
   │     1. List Pending pods по всему кластеру                 │
   │     2. Группировка по пулу через pod_selector              │
   │     3. Для пулов ниже max_nodes (минус in-flight):         │
   │        POST /service/server  →  poll /service/queue        │
   │                                                            │
   │   scaleDown tick (10m)                                     │
   │     1. List nodes; пропустить ноды на min_nodes            │
   │     2. Для idle-нод старше idle_threshold:                 │
   │        cordon → evict (PDB-safe) → POST /server/terminate  │
   │     3. Удалить объект K8s Node                             │
   └─────────────┬───────────────────────────┬──────────────────┘
                 │                           │
        ┌────────▼────────┐           ┌──────▼───────────┐
        │ Kubernetes API  │           │ Kamatera REST    │
        │ (in-cluster или │           │ cloudcli.        │
        │  KUBECONFIG)    │           │ cloudwm.com      │
        └─────────────────┘           └──────────────────┘
```

**Поток scale-up.** Под создаётся с `nodeSelector`. Шедулер K8s не может его
разместить (нет CPU/памяти). За ~30 секунд автоскейлер матчит селектор пода
против сконфигурированного пула, дёргает Kamatera для создания VM с cloud-init
этого пула и помечает ноду как in-flight. Когда VM присоединяется к кластеру
(обычно 2–3 минуты — boot VM + cloud-init), шедулер биндит под.

**Поток scale-down.** Раз в 10 минут автоскейлер проверяет каждую ноду. Если в
её пуле количество нод выше `min_nodes` и на ноде нет workload-подов
(не-DaemonSet), нода кордонится, дренируется (с уважением к
`PodDisruptionBudgets`), VM терминируется через REST API, объект K8s Node
удаляется.

### Почему два API Kamatera

У Kamatera есть два REST API, которые частично перекрываются по функциональности,
но отличаются по форме и надёжности. Автоскейлер использует **оба одновременно**:

| API | Auth | Для чего | Почему |
| --- | --- | --- | --- |
| `cloudcli.cloudwm.com` | заголовки `AuthClientId` + `AuthSecret` | `POST /service/server` (create), `POST /service/server/terminate` | Только в этом API есть поле `script-file`, через которое мы инжектим cloud-init для k3s/RKE2/etc. join. У console API такого поля нет. |
| `console.kamatera.com/service` | Bearer token (POST `/authenticate`) | `GET /queue` (batched recent commands), `GET /servers` (список серверов) | Cloudcli `/service/queue` всегда возвращает `[]` независимо от активности, а `/service/server/info` периодически отдаёт 500. Console-API эквиваленты работают надёжно и позволяют batch-ить K concurrent waiters в один round-trip за poll cycle (избегаем N+1 queue/info calls). |

Одни и те же `KAMATERA_API_CLIENT_ID` / `KAMATERA_API_SECRET` подходят для обоих —
отличается только то, как они передаются в каждый endpoint. Отдельные credentials
не нужны.

---

## Quickstart

После публикации релиза доступен мульти-арх образ (amd64 + arm64) в GHCR:

```sh
docker pull ghcr.io/georges1995/kamatera-autoscaler:latest
```

Минимальный набор манифестов выглядит так. Положите его в свой инфраструктурный
репозиторий, не сюда.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata: { name: kamatera-autoscaler, namespace: kube-system }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: kamatera-autoscaler }
rules:
  - apiGroups: [""]
    resources: ["pods", "nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["delete", "patch", "update"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: kamatera-autoscaler }
subjects:
  - kind: ServiceAccount
    name: kamatera-autoscaler
    namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kamatera-autoscaler
---
apiVersion: v1
kind: Secret
metadata: { name: kamatera-creds, namespace: kube-system }
stringData:
  KAMATERA_API_CLIENT_ID: "..."
  KAMATERA_API_SECRET: "..."
  AUTOSCALER_JOIN_TOKEN: "..."
  SSH_PUB_KEY: "ssh-ed25519 AAAA..."
---
apiVersion: v1
kind: ConfigMap
metadata: { name: kamatera-autoscaler-config, namespace: kube-system }
data:
  pools.yaml: |
    datacenter: EU-FR
    server_ip: 10.0.0.20
    cloud_init_template: /etc/autoscaler/cloud-init.tpl
    pools:
      - name: general
        cpu_type: B
        cpu_cores: 2
        ram_mb: 4096
        disk_gb: 40
        image: ubuntu_server_24.04_64-bit
        min_nodes: 1
        max_nodes: 4
        node_labels: pool=general
        node_taints: ""
        pod_selector: pool=general
  cloud-init.tpl: |
    #cloud-config
    # ... ваш cloud-init template, см. examples/cloud-init.tpl
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: kamatera-autoscaler, namespace: kube-system }
spec:
  replicas: 1
  selector: { matchLabels: { app: kamatera-autoscaler } }
  template:
    metadata: { labels: { app: kamatera-autoscaler } }
    spec:
      serviceAccountName: kamatera-autoscaler
      containers:
        - name: autoscaler
          image: ghcr.io/georges1995/kamatera-autoscaler:latest
          envFrom:
            - secretRef: { name: kamatera-creds }
          env:
            - { name: AUTOSCALER_CONFIG, value: /etc/autoscaler/pools.yaml }
            - { name: LOG_LEVEL, value: info }
          volumeMounts:
            - { name: cfg, mountPath: /etc/autoscaler }
          resources:
            requests: { cpu: 50m, memory: 64Mi }
            limits:   { cpu: 200m, memory: 128Mi }
      volumes:
        - name: cfg
          configMap: { name: kamatera-autoscaler-config }
```

> **Важно:** эти манифесты — пример, они **не лежат** в этом репо. Ваши боевые
> манифесты держите в инфраструктурном репозитории вместе с остальной конфигурацией
> кластера.

---

## Конфигурация

### `pools.yaml`

| Поле | Тип | Описание |
| --- | --- | --- |
| `datacenter` | string | Датацентр Kamatera (например `EU-FR`, `EU`, `US-NY2`). |
| `vlan_name` | string | Опциональное имя приватного VLAN. Если задан — на VM добавляется второй NIC в этом VLAN. |
| `server_ip` | string | IP control-plane, к которому cloud-init будет присоединять агентов (`{{ .ServerIP }}`). |
| `cloud_init_template` | string | Путь к файлу cloud-init template. |
| `pools[]` | list | По одной записи на каждый пул. |
| `pools[].name` | string | Уникальное имя пула; используется как K8s-label `pool=<name>`. |
| `pools[].cpu_type` | string | `A` / `B` / `D` / `T`. См. документацию Kamatera по классам CPU. |
| `pools[].cpu_cores` | int | Количество vCPU. В API передаётся как, например, `"2B"`. |
| `pools[].ram_mb` | int | RAM в MB. |
| `pools[].disk_gb` | int | Основной диск в GB. |
| `pools[].image` | string | Идентификатор образа Kamatera. |
| `pools[].min_nodes` | int | Нижняя граница — автоскейлер не дренирует ниже. |
| `pools[].max_nodes` | int | Верхняя граница — автоскейлер не провижинит выше. |
| `pools[].node_labels` | string | Labels через cloud-init: `key=value` через запятую. |
| `pools[].node_taints` | string | Taints через cloud-init: `key=value:Effect` через запятую. |
| `pools[].pod_selector` | string | Селектор для матчинга против `nodeSelector` пода: определяет какой пул надо растить. |

### Переменные окружения

Бинарь конфигурируется ИСКЛЮЧИТЕЛЬНО через переменные окружения. Файлы `.env`
он не читает; для локалки это делает Makefile-таргет `make run-local`.

| Переменная | Обязательна | Описание |
| --- | --- | --- |
| `KAMATERA_API_CLIENT_ID` | да | Client ID для Kamatera REST API. |
| `KAMATERA_API_SECRET` | да | Secret для Kamatera REST API. |
| `AUTOSCALER_JOIN_TOKEN` | да | Токен присоединения к кластеру, доступен в template как `{{ .JoinToken }}`. |
| `SSH_PUB_KEY` | да | SSH-ключ, инжектируемый в каждую новую VM. |
| `AUTOSCALER_CONFIG` | нет | Путь к `pools.yaml` (default `/etc/autoscaler/pools.yaml`). |
| `LOG_LEVEL` | нет | `debug`, `info`, `warn`, `error` (default `info`). |
| `KUBECONFIG` | нет | Используется только вне кластера; в Pod-е — авто. |

YAML-loader **отвергает** любое из следующих полей в `pools.yaml`:
`kamatera_secret`, `kamatera_client_id`, `api_secret`, `api_client_id`,
`join_token`, `k3s_token`, `ssh_pub_key`, `ssh_key`, `creds`, `credentials`,
`secrets`. Это страховка от случайного коммита кредов в конфигурации кластера.

### Cloud-init template

Template парсится при старте через Go `text/template`. Доступные переменные:

| Переменная | Источник |
| --- | --- |
| `{{ .ServerIP }}` | `config.server_ip` |
| `{{ .JoinToken }}` | env-var `AUTOSCALER_JOIN_TOKEN` |
| `{{ .NodeLabels }}` | `pool.node_labels` пула, который скейлится |
| `{{ .NodeTaints }}` | `pool.node_taints` пула, который скейлится |

Рабочий пример для k3s — [`examples/cloud-init.tpl`](examples/cloud-init.tpl).
Адаптируйте под ваш дистрибутив; автоскейлеру всё равно что делает template,
лишь бы новая VM в итоге появилась в кластере с нужными labels.

---

## Сборка из исходников

```sh
make build         # bin/autoscaler
make image         # локальный kamatera-autoscaler:dev образ
make test          # go test -race ./...
make lint          # golangci-lint; fallback на go vet
```

## Локальный запуск

```sh
cp .env.example .env
$EDITOR .env       # заполните реальные значения; .env в gitignore
export KUBECONFIG=~/.kube/config
make run-local
```

Автоскейлер подключится к кластеру из `KUBECONFIG`, прочитает конфиг из
`examples/pools.yaml` по умолчанию, выведет startup-баннер и будет тикать раз в
30 секунд. Ctrl-C для остановки — graceful shutdown через `signal.NotifyContext`
завершается за пару секунд.

---

## Релизы и Docker-образы

Мульти-арх (amd64 + arm64) образ публикуется в GHCR **только на semver-теги**:

```
ghcr.io/<owner>/kamatera-autoscaler:v1.2.3
ghcr.io/<owner>/kamatera-autoscaler:v1.2
ghcr.io/<owner>/kamatera-autoscaler:v1
ghcr.io/<owner>/kamatera-autoscaler:latest
```

Push в `main` образ **не публикует** — это сознательное решение. Релиз делается так:

```sh
git tag v0.1.0
git push --tags
```

### Использование приватного GHCR-пакета

По умолчанию пакет в GHCR наследует видимость репозитория. Если он приватный,
его всё равно можно тянуть из своих систем:

**Из другого GitHub Actions workflow** (любой репо того же owner/org):

```yaml
permissions:
  packages: read
steps:
  - run: echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin
    env:
      GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
  - run: docker pull ghcr.io/<owner>/kamatera-autoscaler:v0.1.0
```

**Из Kubernetes-кластера** — через `imagePullSecret`:

1. Создайте GitHub PAT со scope `read:packages`.
2. Зарегистрируйте как Kubernetes-secret:
   ```sh
   kubectl create secret docker-registry ghcr-pull \
     --docker-server=ghcr.io \
     --docker-username=<ваш-gh-username> \
     --docker-password=<PAT> \
     --docker-email=<email> \
     -n kube-system
   ```
3. Сошлитесь в Deployment-е:
   ```yaml
   spec:
     template:
       spec:
         imagePullSecrets:
           - name: ghcr-pull
   ```

### Сделать образ публичным

GitHub UI → репо → Packages → `kamatera-autoscaler` → Package settings →
Change visibility → Public. После этого `docker pull` работает без аутентификации.

---

## Тестирование

### Unit-тесты

```sh
make test
```

Fake `kubernetes.Interface` (из `client-go/kubernetes/fake`) и `httptest`-сервер
дают контроллеру детерминированные сценарии для scale-up и scale-down без
обращения к реальной инфраструктуре.

### Native E2E (реальная Kamatera, ~$0.01 за прогон)

End-to-end-тест против живого API Kamatera **в этом репо отсутствует** (он
живёт в `test/e2e/`, который в gitignore — чтобы случайно не закоммитить креды
или артефакты, которые могут что-то насоздавать на чужой счёт). Создайте файлы
локально и запускайте при необходимости отладки REST-клиента. Подробности —
в плане проекта (env-var проверки, `KAMATERA_E2E_CONFIRM`, forced cleanup,
orphan-cleanup-скрипт).

---

## Структура проекта

```
cmd/autoscaler/         entrypoint
internal/config/        YAML schema, валидация, env-only креды
internal/kamatera/      REST-клиент (auth, retries, queue polling)
internal/kubeclient/    интерфейсы поверх client-go (тестируемо)
internal/controller/    scale_up, scale_down, drain, матчинг пулов, in-flight tracker
internal/logging/       slog JSON handler с redact-replacer-ом для секретов
examples/               sample pools.yaml + cloud-init.tpl
.github/workflows/      ci.yml + release.yml (GHCR publish на semver-теги)
```

Каталог `pkg/` намеренно отсутствует — публичного Go API нет.

---

## Лицензия

[MIT](LICENSE).
