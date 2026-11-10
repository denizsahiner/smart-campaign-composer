# Smart Campaign Composer

Pazarlamacı kampanya isteğini düz bir cümleyle yazar. Claude bu isteği bir hedef kitleye
ve SMS / e-posta metinlerine çevirir. Pazarlamacı sonucu inceler, gerekirse düzenler ve
onaylar. Mesajlar bir kuyruk üzerinden sahte (mock) bir sağlayıcıya gönderilir, teslim
durumu ekranda canlı izlenir.

- **Backend:** Go (tek binary, dört rol), PostgreSQL
- **Frontend:** Vue 3 + Vite, API tarafından sunulur
- **LLM:** Claude API (structured outputs); testler ve anahtarsız çalışma için mock istemci

Case metni [`docs/CASE.md`](docs/CASE.md) dosyasında. Tasarım
[`docs/ARCHITECTURE_DRAFT.md`](docs/ARCHITECTURE_DRAFT.md), kararların gerekçeleri
[`DECISIONS.md`](DECISIONS.md) dosyasında anlatılıyor.

## Kurulum

Gereken tek şey Compose v2 ile Docker.

```sh
cp .env.example .env       # isteğe bağlı: Claude için ANTHROPIC_API_KEY ekleyin
docker compose up --build  # ya da: make up
```

Ardından **http://localhost:8080** adresini açın.

- Anahtar yoksa ya da `LLM_PROVIDER=mock` ise API mock LLM ile çalışır. Mock sabit
  cevaplar döner; aşağıdaki örnek istek bilinen bir kitleye ve metne karşılık gelir.
  Claude'u kullanmak için `ANTHROPIC_API_KEY` değerini girip `LLM_PROVIDER=claude` yapın.
- `migrate` servisi ilk açılışta şemayı kurar ve 5.000 sahte kullanıcı yükler (sabit
  random seed ile, her kurulumda aynı veri). Seed'de bilerek konmuş uç durumlar da var:
  çok uzun bir isim, boş isim, telefonu ya da e-postası olmayanlar, izni olmayanlar ve
  sağlayıcının her zaman reddettiği iki alıcı.
- Mock sağlayıcının teslimat kaydı: http://localhost:8090/deliveries
- `make down` sistemi durdurur. Veriyi sıfırdan yüklemek için `docker compose down -v`.

Her ayarın çalışan bir varsayılanı var; hepsi [`.env.example`](.env.example) dosyasında.

### Deneyin

1. **Yeni kampanya** sayfasına şu isteği yazıp **Oluştur**'a basın:
   `İstanbul'da son 30 günde sepetine ürün ekleyip satın almayanlara %30 indirim hatırlatması gönder. Samimi ama kısa olsun.`
2. Hedef kitle, kanal başına kaç kişiye ulaşılacağıyla birlikte gelir (izin filtresi
   uygulanmış haliyle). İstekte kitle kurallarıyla ifade edilemeyen bir şey varsa
   listelenir; devam etmeden önce **Anladım, devam et** ile onaylamanız gerekir.
3. **Mesaj öner**: SMS ve e-posta ekrana akarak gelir, sonra doğrulanır. Metni doğrudan
   düzenleyebilir ya da kısa bir not yazıp **Revize et** ile yeni bir versiyon
   isteyebilirsiniz.
4. **Onayla ve gönder**: mesajlar kuyruğa alınır, teslim durumu canlı güncellenir.
5. **İzleme** sayfası kuyruk derinliğini, mesaj sayılarını, LLM çağrı istatistiklerini,
   worker ve sağlayıcının sağlık durumunu gösterir.

## Mimari

```
            ┌──────────── tarayıcı (Vue) ─────────────┐
            │  HTTP + SSE (mesaj akışı, teslimat)     │
            ▼                                         │
┌────────────────────┐   Claude API / mock LLM        │
│ api  :8080         │────────────────────────▶       │
│ REST, SSE, web UI  │                                │
└─────────┬──────────┘                                │
          │ campaigns, messages (= kuyruk), llm_calls
          ▼
┌────────────────────┐        ┌─────────────────────┐
│ postgres           │◀──────▶│ worker              │
└────────────────────┘ claim  │ kanal başına döngü  │
          ▲           /sonuç  │ hız limiti, retry   │
          │                   └──────────┬──────────┘
   migrate (tek seferlik:                │ POST /sms, /email
   şema + seed)                          ▼
                              ┌─────────────────────┐
                              │ mock-provider :8090 │
                              │ 5xx, 429, timeout   │
                              └─────────────────────┘
```

Aynı Go binary'si (`cmd/app`) `migrate`, `api`, `worker` ve `mock-provider` olarak, her
biri kendi container'ında çalışır. Ortak durum yalnızca Postgres'te tutulur;
`messages` tablosu kuyruğun kendisidir, ayrı bir message broker yok.

### LLM: karar değil, öneri

Claude'un çıktısı SQL'e ya da bir alıcıya ulaşmadan önce her zaman backend'den geçer.

- **İki adım, iki JSON şeması.** *Kitle* adımı kitle kurallarını, *mesaj* adımı her dil
  için SMS'i, e-posta konusunu ve gövdesini döner. İkisi de structured outputs kullanır, yani cevap
  her zaman parse edilebilir JSON'dur.
- **Kitle kuralları SQL değil, beyaz listeli bir DSL.** Yalnızca dört alana izin var
  (`city`, `language`, `cart_added_days_ago`, `purchased_after_cart`); her birinin kendi
  operatörleri ve değer aralıkları var. Bir query builder bunları parametreli SQL'e
  çevirir. İzin (consent) hiçbir zaman bir kural olamaz: kanal başına izin filtresi her
  zaman kodda uygulanır.
- **İş kuralları her cevapta kontrol edilir.** Beyaz liste, bilinen şehirler, değer
  aralıkları, SMS ≤ 160 ve konu ≤ 80 karakter (Unicode karakter olarak sayılır;
  `{{first_name}}` o dildeki alıcıların en uzun ismi kadar hesaplanır) ve tek izinli
  değişkenin `{{first_name}}` olması.
- **Her alıcı kendi dilinde mesaj alır.** Pazarlamacı brief'i istediği dilde yazar;
  mesajlar kitlede ulaşılabilir alıcısı olan her dil (Türkçe, İngilizce) için tek çağrıda,
  her dilde ayrı ayrı yazılır, birbirinin çevirisi olarak değil. Kampanya sayfasında her
  dil kendi sekmesindedir. Revize ederken hangi dillerin yeniden yazılacağı seçilir;
  seçilmeyen diller korunur ve tutarlılık için Claude'a bağlam olarak verilir. Onayda
  her alıcıya `users.language` değerine göre o dilin metni gider.
- **Düzeltme döngüsü.** Kurala uymayan cevap somut hatalarla Claude'a geri gönderilir
  (ör. "SMS 178 karakter, en az 18 kısalt"). Her adımda en fazla 2 düzeltme turu ve
  bunlardan bağımsız olarak en fazla 2 bağlantı tekrarı (backoff ve `retry-after` ile)
  yapılır; yani bir adım en fazla 5 çağrı yapar. Mesaj taslağı bunlardan sonra hâlâ
  geçersizse, hatalı alanlar işaretlenmiş olarak düzenlemesi için pazarlamacıya verilir.
- **Her çağrı kaydedilir.** `llm_calls` tablosuna ham cevap, doğrulama hataları, deneme
  numaraları, token sayıları ve süre yazılır. `/api/metrics` bu kayıtlardan beslenir.
- **Mock LLM.** `LLMClient` bir arayüz. Testler, API anahtarı olmadan doğrula → düzelt →
  vazgeç yollarını sırayla senaryolanmış bir mock ile çalıştırır.

### Gönderim: her mesaj en fazla bir kez teslim edilir

- **Onay tek bir transaction.** Son içerik aynı doğrulayıcıdan geçer, kampanya koşullu
  bir update ile `ready → approved` olur. Kişiselleştirilmiş mesajlar Go'da üretilip
  `UNIQUE (campaign_id, user_id, channel)` kısıtıyla eklenir. Çift tıklama ya da ikinci
  sekme `409` alır, hiçbir şey iki kez kuyruğa girmez.
- **Zamanlanmış gönderim.** Onayda "İleri tarihte gönder" seçilirse alıcılar ve metinler
  yine onay anında sabitlenip kuyruğa yazılır; yalnızca her mesajın `next_attempt_at`
  değeri seçilen zamana ayarlanır ve kampanya `scheduled` olur. Zaman İstanbul saatiyle
  seçilir: en az 1 dakika, en çok 14 gün sonrası ve 09:00–21:00 arası (sessiz saatlere
  zamanlanamaz). Worker ilk mesajı aldığı SQL ifadesinde kampanyayı `approved` yapar, ayrı
  bir zamanlayıcı yok. Gönderim başlamadan `POST /api/campaigns/{id}/schedule/cancel`
  bekleyen mesajları siler ve kampanyayı taslaklarıyla `ready`'ye döndürür; başladıysa
  `409 send_started`.
- **Worker.** Her kanalın kendi döngüsü var: lease ile `FOR UPDATE SKIP LOCKED`, saniyede
  100 mesaj hız limiti ve fencing'li sonuç yazımı. Bir worker ölürse satırları lease
  süresi dolunca tekrar alınır.
- **Retry.** 5xx ve timeout'lar jitter'lı üstel backoff ile en fazla 5 denemeye kadar
  tekrarlanır. 429 kanalı `Retry-After` süresi kadar durdurur ve deneme sayılmaz.
  Geçersiz alıcı ilk denemede kalıcı olarak başarısız olur.
- **Gönderilemeyen alıcılar.** Sağlayıcının kalıcı olarak reddettiği alıcılar (geçersiz
  telefon ya da e-posta) kampanya sayfasında ad, kanal, adres ve nedenle listelenir ve
  Excel'de açılan bir CSV olarak indirilebilir
  (`GET /api/campaigns/{id}/failures`, `…/failures.csv`). Denemeleri tükenen geçici
  hatalar listelenmez; alıcının verisinde bir sorun yoktur.
- **Idempotency.** Mesaj ID'si idempotency anahtarı olarak gönderilir; sağlayıcı tekrar
  gelen isteğe yeniden göndermek yerine `duplicate: true` döner. Mock sağlayıcı bazen
  mesajı teslim edip sonra cevap vermeden takılır; tam olarak bu durum test edilmiş olur.
  `/deliveries` her mesajı en fazla bir kez listeler.

### İzleme

- Her serviste `GET /healthz`; bir kanal döngüsü takılırsa worker sağlıksız görünür.
- `GET /api/metrics`: kuyruk derinliği, duruma göre mesaj sayıları, LLM istatistikleri,
  worker ve sağlayıcının canlı sağlık durumu. Bunlardan biri çökmüş olsa da cevap verir.
- `GET /api/campaigns/{id}/events`: kampanya başına teslim sayıları ve gönderim hızını
  veren SSE akışı; kampanya tamamlanınca kapanır.

### Loglar

- Her servis stdout'a JSON log yazar (`LOG_FORMAT=text` ile düz metin). Her satırda
  `service` alanı vardır; kampanyaya ait satırlarda `campaign_id`, mesaja ait satırlarda
  `message_id` bulunur. Böylece bir kampanyanın API'den worker'a kadar bütün kaydı tek
  bir alanla filtrelenebilir:
  `docker compose logs api worker --no-log-prefix | jq 'select(.campaign_id == 6)'`.
- Her istek bir request ID alır ve bu ID `X-Request-ID` header'ında döner. 500 cevabında
  ID ayrıca gövdede bulunur; arayüz bunu "Hata kodu" olarak gösterir.
- API ve worker'ın **uyarı ve hataları** ayrıca Postgres'teki `app_logs` tablosuna
  yazılır. Yazma, istekleri ve gönderimleri bekletmez: kayıtlar arka planda toplu olarak
  eklenir. Kayıtlar `LOG_RETENTION_DAYS` (varsayılan 7) gün sonra silinir.
- **Loglar** sayfasında (`/logs`, İzleme'den açılır) bu kayıtlar seviye, servis, kampanya,
  hata kodu ya da metinle filtrelenir. İzleme sayfası son 24 saatin hata ve uyarı sayısını
  gösterir. Kampanya sayfası, o kampanya için hata kaydedildiyse bir uyarı ve loglara
  giden bir bağlantı gösterir.
- Bilgi seviyesindeki loglar (her istek, her LLM adımının sonucu) DB'ye yazılmaz; yalnızca
  `docker compose logs` ile görülür.

## Testler

```sh
make test        # Go testleri + frontend testleri (Vitest)
```

API anahtarı gerekmez, LLM mock'lanır. Go 1.26+ ve Node 18+ gerekir.

Veritabanı entegrasyon testleri (onay, worker claim/lease, izin filtresi, mock sağlayıcıya
karşı uçtan uca gönderim) `make test` ile birlikte koşar: `make test-go`, compose'daki
Postgres'i kendisi başlatır, testlerden sonra (kendisi başlattıysa) durdurur. Gereken tek
şey Docker. Docker yoksa test sessizce atlanmaz, net bir hata verir. Kendi Postgres'inizi
kullanmak için `TEST_DATABASE_URL` verin; o zaman container başlatılmaz. Her test paketi
kendi geçici veritabanını açar ve sonunda siler; sistemdeki veriye dokunulmaz.
Veritabanı olmadan, entegrasyon testleri atlanarak koşmak için: `make test-db-less`.

Tek bir istek için iki LLM adımını gerçek API'ye karşı denemek için (Go container'ında
çalışır):

```sh
make llm-try BRIEF="İstanbul'da son 30 günde sepetini terk edenlere %30 indirim"
```

## Geliştirme

```sh
docker compose up -d postgres mock-provider worker
DATABASE_URL="postgres://composer:composer@localhost:5433/composer?sslmode=disable" go run ./cmd/app api
cd web && npm install && npm run dev    # http://localhost:5173, /api isteklerini :8080'e yönlendirir
```

## Klasör yapısı

```
cmd/app/            giriş noktası: migrate | api | worker | mock-provider | healthcheck | llm-try
internal/llm        Claude ve mock istemciler, hata sınıflandırma
internal/prompt     adım prompt'ları, JSON şemaları, düzeltme istekleri
internal/compose    düzeltme ve bağlantı tekrarı bütçeleriyle LLM adımları
internal/logging    JSON logger, istek kapsamı (request_id, campaign_id), DB'ye log yazımı
internal/segment    kitle DSL'i, doğrulama, SQL query builder
internal/content    içerik doğrulama ve alıcı başına metin üretimi
internal/api        HTTP handler'lar, SSE akışları, metrikler
internal/store      Postgres erişimi (campaigns, messages, llm_calls)
internal/worker     claim döngüleri, hız limiti, retry
internal/provider   mock SMS/e-posta sağlayıcı
internal/seed       deterministik sahte kullanıcılar ve olaylar
web/                Vue frontend ve testleri
ai-sessions/        Claude Code oturum kayıtları
```

## Yapılmayanlar / bilinen sınırlamalar

- **Bonus özellikler:** A/B varyantları ve CI yapılmadı.
- **Zamanlanmış kampanyanın kitlesi onayda donar.** Onay ile gönderim arasında kitleye
  giren ya da izni değişen kişiler hesaba katılmaz (en fazla 14 gün).
- **İki dil.** Mesajlar Türkçe ve İngilizce yazılır (`users.language` yalnızca bu iki
  değeri alabiliyor). Arayüz metinleri Türkçe.
- **SMS uzunluğu** case'teki 160 karakter kuralına göre kontrol ediliyor. GSM-7 / UCS-2
  kodlaması (Türkçe karakterlerin gerçek limiti 70'e düşürmesi) kapsam dışı.
- **Kitle kuralları** yalnızca AND ile birleşir (OR / NOT yok) ve arayüzde kitle
  düzenleyici yok. İstek ifade edilemiyorsa pazarlamacı isteği değiştirir.
- **Tek replika.** "Kampanya başına aynı anda tek LLM adımı" kilidi bellekte tutuluyor;
  mock sağlayıcının idempotency hafızası yeniden başlatınca sıfırlanıyor.
- **Kimlik doğrulama yok.** Tek pazarlamacılı bir demo.
