# vkdl.link

Hospedagem de imagens simples, anônima e de código aberto. Envie uma imagem e
receba um link público, mais um token para apagá-la depois. É o destino padrão
dos envios do [Recortei](https://github.com/FabioHAraujo/Recortei).

- Instância pública: <https://vkdl.link>
- Só imagens rasterizadas: **PNG, JPEG, GIF e WebP**, identificadas pelos bytes
  do arquivo (nunca pelo nome ou Content-Type). SVG e qualquer outra coisa são recusados.
- Sem IP guardado: logs e banco só têm um HMAC do IP.
- Um único binário Go + SQLite. As imagens ficam no disco ou, opcionalmente,
  num bucket compatível com S3 (AWS, iDrive e2, MinIO, Garage...).

## Retenção: por quanto tempo a imagem fica?

Envios **anônimos** são gratuitos e **não têm prazo garantido**. O limite é o
armazenamento: quando o orçamento (`VKDL_MAX_STORAGE`) é ultrapassado, ou (no
armazenamento em disco) o espaço livre no volume cai abaixo de
`VKDL_MIN_FREE_DISK`, os arquivos despejáveis **mais antigos** são apagados
primeiro. Nunca prometemos um tempo.

**Chaves de API** têm um período de retenção garantido (`retention_days`; 0 =
para sempre). Um arquivo enviado com chave não é despejado até `expires_at`;
depois disso ele entra na fila de despejo como qualquer anônimo (mais antigos
primeiro). Envios com chave também têm um limite de taxa maior.

Quem tem o `delete_token` de um arquivo pode apagá-lo. O administrador pode
apagar qualquer arquivo (pedidos de remoção, LGPD).

## API

Todos os erros são JSON: `{"error":"<código>","message":"<texto>"}`.
A especificação completa está em [`openapi.yaml`](openapi.yaml).

### Enviar

```sh
curl -F "file=@captura.png" https://vkdl.link/v1/upload
```

Também aceita o corpo cru (qualquer Content-Type):

```sh
curl --data-binary @captura.png https://vkdl.link/v1/upload
```

Resposta `201`:

```json
{
  "id": "aB3dE9xQ",
  "url": "https://vkdl.link/aB3dE9xQ.png",
  "delete_url": "https://vkdl.link/v1/files/aB3dE9xQ",
  "delete_token": "…",
  "size": 12345,
  "mime": "image/png",
  "expires_at": null
}
```

`expires_at` é `null` para envios anônimos (sem garantia) e para chaves com
`retention_days` 0 (para sempre); senão é a data RFC 3339 até a qual o arquivo
está protegido.

Com chave de API: `-H "Authorization: Bearer vk_…"`. Chave inválida ou
revogada devolve `401` (o envio **não** vira anônimo em silêncio).

| status | quando |
|---|---|
| 400 | nenhum arquivo enviado / corpo ilegível |
| 401 | chave de API inválida ou revogada |
| 413 | arquivo maior que `VKDL_MAX_UPLOAD` |
| 415 | não é PNG, JPEG, GIF ou WebP |
| 429 | limite de envios por minuto (veja `Retry-After`) |
| 507 | sem espaço e nada que possa ser despejado |

### Baixar

```sh
curl -O https://vkdl.link/aB3dE9xQ.png   # também: https://vkdl.link/aB3dE9xQ
```

`GET` e `HEAD` com `Range`, `ETag`/`If-None-Match`, cache imutável de um ano e
cabeçalhos restritivos (`nosniff`, CSP, `Referrer-Policy: no-referrer`).
Extensão errada para o id dá `404`.

### Apagar

```sh
curl -X DELETE -H "X-Delete-Token: <token>" https://vkdl.link/v1/files/aB3dE9xQ
```

`204` apagado, `403` token errado ou ausente, `404` não existe.

Atrás de uma CDN, a cópia em cache só some se o cache for limpo; veja
[Cache da CDN e remoção de arquivos](#cache-da-cdn-e-remoção-de-arquivos).

### Admin

Habilitado só quando `VKDL_ADMIN_TOKEN` está definido (senão as rotas dão `404`).

```sh
ADM="Authorization: Bearer $VKDL_ADMIN_TOKEN"

# criar chave (o segredo "key" aparece só nesta resposta)
curl -H "$ADM" -d '{"name":"cliente X","retention_days":30}' https://vkdl.link/v1/admin/keys

# listar (sem segredos) e revogar
curl -H "$ADM" https://vkdl.link/v1/admin/keys
curl -H "$ADM" -X DELETE https://vkdl.link/v1/admin/keys/<id>

# números: arquivos, bytes, orçamento, disco livre (só no armazenamento em disco), bytes despejáveis
curl -H "$ADM" https://vkdl.link/v1/admin/stats

# apagar qualquer arquivo
curl -H "$ADM" -X DELETE https://vkdl.link/v1/files/<id>
```

### Outros

- `GET /` página explicativa (pt-BR e inglês).
- `GET /healthz` → `{"status":"ok"}` (verifica o banco).
- CORS `Access-Control-Allow-Origin: *` nos arquivos e em `/v1/upload` (com preflight `OPTIONS`).

## Configuração

Tudo por variáveis de ambiente:

| variável | padrão | significado |
|---|---|---|
| `VKDL_LISTEN` | `:8080` | endereço de escuta |
| `VKDL_BASE_URL` | `http://localhost:8080` | origem pública usada para montar os links (produção: `https://vkdl.link`) |
| `VKDL_DATA_DIR` | `./data` (no container: `/data`) | SQLite, temporários de envio e (com `fs`) os arquivos |
| `VKDL_STORAGE` | `s3` se `S3_BUCKET` estiver definido, senão `fs` | onde ficam as imagens; veja [Armazenamento](#armazenamento-disco-ou-s3) |
| `VKDL_MAX_STORAGE` | `10GB` | orçamento total de bytes (aceita KB/MB/GB/TB, base 1024; `0` desliga) |
| `VKDL_MIN_FREE_DISK` | `1GB` | só com `fs`: despeja quando o espaço livre do volume cai abaixo disso (`0` desliga) |
| `VKDL_MAX_UPLOAD` | `20MB` | tamanho máximo de um envio |
| `VKDL_ADMIN_TOKEN` | vazio (admin desligado) | bearer token de `/v1/admin/*` |
| `VKDL_SECRET` | aleatório a cada boot (com aviso) | segredo do HMAC dos IPs |
| `VKDL_RATE_ANON` | `30` | envios por minuto por IP (anônimo; `0` desliga) |
| `VKDL_RATE_KEY` | `300` | envios por minuto por chave de API (`0` desliga) |
| `VKDL_TRUSTED_PROXY_HEADER` | `CF-Connecting-IP` | cabeçalho com o IP real do cliente, aceito **só** quando a conexão vem de uma faixa de `VKDL_TRUSTED_PROXIES` |
| `VKDL_TRUSTED_PROXIES` | faixas da Cloudflare | CIDRs (separados por vírgula) da borda que pode informar o IP pelo cabeçalho acima; `none` desliga |
| `VKDL_CF_ZONE_ID` | vazio | zona da Cloudflare; com o token abaixo, ao apagar um arquivo o cache dele é limpo na borda |
| `VKDL_CF_API_TOKEN` | vazio | token da API da Cloudflare só com a permissão *Zone → Cache Purge → Purge* (defina os dois ou nenhum) |

Gere segredos fortes com `openssl rand -hex 32`. Os tokens de exclusão e as
chaves de API são guardados como SHA-256 e **não** dependem de `VKDL_SECRET`;
trocar o segredo só muda os hashes de IP.

> **Como o IP do cliente é descoberto.** Primeiro o servidor acha a *borda*:
> o endereço da conexão, ou, quando ela vem de um proxy interno (rede privada,
> como o Traefik do Coolify), o **último** salto do `X-Forwarded-For`, que é
> escrito pelo próprio proxy e o cliente não controla. Só se essa borda estiver
> em `VKDL_TRUSTED_PROXIES` (por padrão, as faixas publicadas da Cloudflare) o
> `CF-Connecting-IP` é usado. Quem fala direto com a origem e forja o cabeçalho
> é limitado pelo próprio IP. Sem Cloudflare na frente, aponte
> `VKDL_TRUSTED_PROXIES` para o seu proxy e `VKDL_TRUSTED_PROXY_HEADER` para o
> cabeçalho que ele sempre sobrescreve. O limitador guarda no máximo 100 mil
> IPs; acima disso os IPs novos dividem um único balde.

## Armazenamento: disco ou S3

Os **metadados** (arquivos, chaves de API) ficam sempre no SQLite em
`VKDL_DATA_DIR/vkdl.db`, e os envios passam por `VKDL_DATA_DIR/tmp` antes de
irem para o destino. Por isso `/data` continua precisando de um volume
persistente mesmo com S3. Já as **imagens** podem ficar em dois lugares:

- **`fs`** (padrão sem `S3_BUCKET`): em `VKDL_DATA_DIR/files/<id[0:2]>/<id>.<ext>`.
  O despejo considera `VKDL_MAX_STORAGE` e `VKDL_MIN_FREE_DISK`.
- **`s3`** (padrão quando `S3_BUCKET` está definido): num bucket compatível com
  S3, em `<S3_PREFIX><id[0:2]>/<id>.<ext>` (o mesmo layout do disco). O disco
  local só guarda o banco e os temporários, então **só `VKDL_MAX_STORAGE`**
  limita o tamanho; `VKDL_MIN_FREE_DISK` é ignorado. O bucket pode (e deve)
  ser privado: as imagens são servidas pelo próprio vkdl.

| variável | padrão | significado |
|---|---|---|
| `S3_ENDPOINT` | — (obrigatória) | host do serviço, ex. `s3.us-east-1.idrivee2.com`; aceita também `http://host:porta` ou `https://host`, e aí o esquema define o TLS |
| `S3_REGION` | vazio (descobre sozinho) | região do bucket, ex. `us-east-1` |
| `S3_BUCKET` | — (obrigatória) | nome do bucket (precisa existir) |
| `S3_KEY_ID` | — (obrigatória) | access key id; nunca vai para o log |
| `S3_KEY_SECRET` | — (obrigatória) | secret access key; nunca vai para o log |
| `S3_PREFIX` | vazio | "pasta" dentro do bucket, ex. `vkdl/` |
| `S3_USE_SSL` | `true` | `false` para endpoints HTTP (MinIO/Garage na rede interna) |
| `S3_PATH_STYLE` | `auto` | `true` = `endpoint/bucket/chave` (MinIO, Garage); `false` = `bucket.endpoint/chave`; `auto` escolhe sozinho (virtual na AWS, caminho no resto) |

No boot o servidor confere se o bucket responde (até 15 s) e **não sobe** se
não conseguir, com uma mensagem que cita a variável a conferir (nunca o valor
das credenciais). O log de boot mostra o endpoint, o bucket e o prefixo em uso.

> **O bucket (ou o prefixo) é exclusivo da instância.** No boot, a
> reconciliação apaga linhas do banco cujo objeto sumiu e também objetos no
> formato `<id[0:2]>/<id>.<ext>` dentro do prefixo que não têm linha no banco e
> têm mais de 10 minutos. Duas instâncias com bancos diferentes no mesmo
> bucket+prefixo apagariam os arquivos uma da outra. Objetos fora desse formato
> nunca são tocados. Se o armazenamento estiver vazio e o banco não, nada é
> apagado (quase sempre é bucket/prefixo errado) e o log registra um erro.
>
> O contrário também é protegido: se o **banco estiver vazio** (volume novo,
> `VKDL_DATA_DIR` errado) ou não conhecer **mais da metade** dos objetos
> listados (backup antigo do banco, banco de outra instância), nenhum objeto é
> apagado e o log registra um erro. Confira o volume e o bucket/prefixo antes
> de reiniciar; os objetos sem linha não aparecem no site, mas continuam lá.

Com S3, o envio dos bytes ao bucket acontece fora da fila do despejo, com um
prazo proporcional ao tamanho (20 s + 1 s a cada 256 KiB, no máximo 2 min).
Se o bucket não responder a tempo, o cliente recebe `503 storage_timeout` e
nada fica gravado; GET/HEAD de arquivos desistem em segundos (15 s pelos
cabeçalhos, até 3 tentativas) em vez de ficarem presos. Requisições com várias
faixas (`Range: bytes=0-0,5-5,...`) recebem o arquivo inteiro com 200, e cada
GET no bucket pede no máximo 4 MiB.

Exemplos:

```sh
# iDrive e2 (o endpoint e a região aparecem no painel do bucket)
S3_ENDPOINT=s3.us-east-1.idrivee2.com
S3_REGION=us-east-1
S3_BUCKET=meu-bucket
S3_KEY_ID=...
S3_KEY_SECRET=...

# MinIO na mesma rede Docker
S3_ENDPOINT=http://minio:9000
S3_REGION=us-east-1
S3_BUCKET=vkdl
S3_PATH_STYLE=true
S3_KEY_ID=...
S3_KEY_SECRET=...

# Garage (região "garage" por padrão no garage.toml)
S3_ENDPOINT=http://garage:3900
S3_REGION=garage
S3_BUCKET=vkdl
S3_PATH_STYLE=true
S3_KEY_ID=GK...
S3_KEY_SECRET=...
```

**Migrando de `fs` para `s3`:** pare o servidor, copie o conteúdo de
`/data/files` para dentro do prefixo do bucket mantendo os caminhos (ex.:
`rclone copy /data/files remoto:bucket/prefixo` ou
`aws s3 sync /data/files s3://bucket/prefixo --endpoint-url https://...`), defina
as variáveis `S3_*` e suba de novo com o mesmo `/data` (o banco continua lá).
Linhas cujo arquivo não foi copiado são removidas na reconciliação do boot;
depois de conferir, `/data/files` pode ser apagado.

## Hospedando você mesmo (Docker)

```sh
docker build -t vkdl .
docker volume create vkdl-data
docker run -d --name vkdl -p 8080:8080 -v vkdl-data:/data \
  -e VKDL_BASE_URL=https://img.exemplo.com \
  -e VKDL_SECRET="$(openssl rand -hex 32)" \
  -e VKDL_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  vkdl
```

A imagem roda como usuário sem privilégios (uid 10001), declara `VOLUME /data`
e já traz `/data` com o dono certo, então um volume nomeado novo funciona sem
`chown`. Tem `HEALTHCHECK` com `wget` em `/healthz`. Ao receber `SIGTERM` o
servidor termina as requisições em andamento antes de sair.

Com S3, acrescente as variáveis `S3_*` (por exemplo com `--env-file`); o
volume em `/data` continua necessário para o banco.

No Coolify: aplicação a partir do repositório usando o `Dockerfile`, porta
`8080`, volume persistente em `/data`, healthcheck em `/healthz` e as variáveis
acima (pelo menos `VKDL_BASE_URL`, `VKDL_SECRET` e `VKDL_ADMIN_TOKEN`, mais as
`S3_*` se for usar S3).

### Restringindo a origem à Cloudflare (opcional)

O servidor já ignora o `CF-Connecting-IP` de quem não vem da Cloudflare. Para
fechar a origem de vez, ela pode responder só aos IPs
da Cloudflare (lista oficial em <https://www.cloudflare.com/ips/>). Escolha uma
das opções:

- **Middleware do Traefik (Coolify)**: nos *Custom Labels* da aplicação,
  crie um `ipAllowList` com as faixas da Cloudflare e ligue-o ao roteador
  HTTPS que o Coolify gerou (troque `<router>` pelo nome que aparece nos
  labels existentes):

  ```
  traefik.http.middlewares.cf-only.ipallowlist.sourcerange=173.245.48.0/20,103.21.244.0/22,103.22.200.0/22,103.31.4.0/22,141.101.64.0/18,108.162.192.0/18,190.93.240.0/20,188.114.96.0/20,197.234.240.0/22,198.41.128.0/17,162.158.0.0/15,104.16.0.0/13,104.24.0.0/14,172.64.0.0/13,131.0.72.0/22,2400:cb00::/32,2606:4700::/32,2803:f800::/32,2405:b500::/32,2405:8100::/32,2a06:98c0::/29,2c0f:f248::/32
  traefik.http.routers.<router>.middlewares=cf-only
  ```

  Se o roteador já tiver middlewares, acrescente `cf-only` à lista separada
  por vírgulas. Confira a lista de faixas de tempos em tempos.
- **Firewall do host**: libere as portas 80/443 só para as faixas da
  Cloudflare (ex.: `ufw`/`nftables`). Isso protege todas as aplicações do
  servidor de uma vez.

### Cache da CDN e remoção de arquivos

Os arquivos são servidos com `Cache-Control: public, max-age=31536000, immutable`,
e a Cloudflare guarda imagens (`.png`, `.jpg`, `.gif`, `.webp`) em cache por
padrão respeitando esse prazo. Apagar um arquivo remove o conteúdo da origem,
mas a cópia da borda **continua no ar** até expirar, a menos que o cache seja
limpo. Para que `DELETE /v1/files/{id}` (inclusive remoções por LGPD) tire o
arquivo do ar de verdade, defina `VKDL_CF_ZONE_ID` e `VKDL_CF_API_TOKEN`: a cada
exclusão o servidor pede à Cloudflare a purga de `<base>/<id>.<ext>` e
`<base>/<id>` (em segundo plano, sem atrasar a resposta; falhas vão para o log,
sem o token). Sem essas variáveis, faça a purga manualmente no painel da
Cloudflare (*Caching → Configuration → Purge Cache → Custom Purge*). Arquivos
despejados por falta de espaço não são purgados.

### Sem Docker

```sh
go build -o vkdl ./cmd/vkdl
VKDL_BASE_URL=http://localhost:8080 ./vkdl
```

## Desenvolvimento

```sh
go vet ./...
go test ./...
```

O teste de integração do S3 só roda com `VKDL_TEST_S3=1` e usa as mesmas
variáveis `S3_*` do servidor. Ele grava tudo num subprefixo aleatório
(`<S3_PREFIX>it-<aleatório>/`), apaga no fim e confere que não sobrou nada:

```sh
VKDL_TEST_S3=1 S3_ENDPOINT=http://127.0.0.1:9000 S3_BUCKET=vkdl-test \
  S3_KEY_ID=... S3_KEY_SECRET=... S3_PREFIX=vkdl-ci-test/ \
  go test ./internal/store/ -run S3Integration -v
```

Estrutura:

```
cmd/vkdl/            main: configuração, boot, desligamento gracioso
internal/config/     variáveis de ambiente e tamanhos ("20MB")
internal/db/         SQLite (modernc.org/sqlite, sem CGO): arquivos e chaves
internal/store/      armazenamento (interface; em disco e em S3 com minio-go)
internal/janitor/    despejo por orçamento/disco e reconciliação no boot (linhas e objetos órfãos)
internal/ratelimit/  limite de envios por minuto
internal/cdn/        purga do cache da Cloudflare ao apagar
internal/sniff/      detecção de formato por bytes mágicos
internal/ident/      ids, tokens e hashes
internal/server/     rotas e handlers HTTP, página inicial
```

## Licença

[AGPL-3.0](LICENSE). Se você rodar uma versão modificada como serviço público,
precisa disponibilizar o código-fonte dela.
