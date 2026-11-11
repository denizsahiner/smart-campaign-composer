# Kararlar

Projede dikkatimi çeken, birden fazla yolu olan kararları ve neden birini seçtiğimi buraya
yazdım. Tasarımın tamamı [docs/ARCHITECTURE_DRAFT.md](docs/ARCHITECTURE_DRAFT.md)
dosyasında.

## Genel

### Kuyruk Postgres'te, ayrı bir broker yok

`messages` tablosu kuyruğun kendisi, ayrı bir kuyruk sistemi yok. Onayda her (kullanıcı,
kanal) için bu tabloya bir satır ekleniyor, worker satırları `FOR UPDATE SKIP LOCKED` ile
alıyor. Onay ve kuyruğa alma aynı transaction'da oluyor.

<!-- TODO: Postgres'i kuyruk seçme gerekçeni buraya kendi cümlelerinle yaz (oturumlarda
bu konu hiç tartışılmadı, taslakta da sebep yazmıyor). -->

### Tek Go binary, her rol ayrı container

`api`, `worker`, `mock-provider` ve `migrate` aynı binary'nin farklı komutları. Aynı model,
LLM istemcisi ve doğrulama kodu paylaşılıyor, kopyalamaya gerek kalmıyor. Ama her biri kendi
container'ında çalışıyor, böylece biri çökerse diğerleri etkilenmiyor. `migrate` tek
seferlik çalışıp şemayı kuruyor ve seed'i yüklüyor, API yeniden başlayınca migration
tekrar çalışmıyor.

### Consent (izin) kitle kuralı değil, kodda sabit filtre

Claude'un ürettiği kitle kuralında `sms_consent` ya da `email_consent` olamaz; olursa
doğrulama reddeder. Backend her zaman kanal başına sabit bir filtre ekler (SMS için
`sms_consent AND phone IS NOT NULL`, e-posta için `email_consent AND email IS NOT NULL`).
Neden: izin bir hedefleme tercihi değil, bir uyumluluk (compliance) zorunluluğu. LLM'in onu
gevşetememesi ya da atlayamaması gerekiyor. Bedeli: "SMS izni olmayanlara e-posta at" gibi brief'ler ifade
edilemiyor, bu kapsam için kabul ettim.

### LLM sonucu HTTP hatası değil, cevabın içinde döner

"Brief eşlenemedi", "düzeltme hakları bitti", "Claude meşgul" gibi durumlar `200/201`
cevabında `audience_error` olarak geliyor (akışta `error` event'i). Bozuk istek, olmayan
kampanya, onaylanmış kampanyayı değiştirme gibi gerçek istek hataları ise
`400/404/409/422`, bizim hatalarımız `500` döner. Alternatif, bunlar için de HTTP kodu
(422/503) dönmekti; LLM sonuçları için cevap içinde dönmeyi seçtim. Claude meşgulse
kampanya hiç değişmiyor; brief kaynaklı bir hatada yeni brief kaydediliyor ve eski kitle
temizleniyor.

### Mesaj akışı POST + SSE, `EventSource` değil

Alternatif `GET` + `EventSource` idi: tarayıcı desteği daha kolay, ama içerik üreten bir
işlem için `GET` kullanmak anlamca yanlış. Bu yüzden `POST` seçtim ve frontend akışı `fetch`
ile okuyor. Akış
başladıktan sonra durum kodu zaten 200 gönderilmiş oluyor, bu yüzden o andan sonraki hatalar
SSE `error` event'i olarak gelir. Ön koşullar (kitle yok, onaylanmamış uyarı) akış
başlamadan HTTP koduyla döner.

### Kampanya başına tek LLM adımı, kilit bellekte

Aynı kampanyada ikinci bir LLM isteği `409 busy` alır. Seçenekler: bellekte kilit, Postgres
advisory lock, veritabanında lease kolonu, kilitsiz "son yazan kazanır". Bellekte kilidi
seçtim: en basiti, ek tablo ya da bağlantı gerektirmiyor. Bedeli sadece tek API replikasında
doğru olması. Kilitsiz yol ücretli LLM çağrısını iki kez yapar ve iki sekme farklı taslak
görür. Bu sınırı README'de de yazdım. Kullanıcı bağlantıyı keserse LLM çağrısı iptal oluyor
ve yarım taslak kaydedilmiyor.

### Kimlik doğrulama yok

API'de kimlik doğrulama yok, çünkü bunu tek pazarlamacılı bir demo olarak kurdum. README'de
"yapılmayanlar"da yazıyor. Mock sağlayıcıda da kimlik doğrulama yok; ona sahte bir
`PROVIDER_API_KEY` eklemedim.

### Kampanya durumları

`draft → ready → scheduled → approved → completed`. Kampanya ancak `draft` ya da `ready`
iken değiştirilebilir. Onaylanmış bir kampanyayı hiçbir endpoint değiştiremiyor; bunu
veritabanındaki koşullu `UPDATE`'ler sağlıyor, yani iki sekme ya da çift tıklama güvenli.
Yeni bir kitle oluşturulursa taslak ve onay sıfırlanır, çünkü hepsi kitleye bağlı.

## LLM

### LLM istemcisi ham cevabı döner, kontrolü çağıran yapar

`llm.Client` modelin cevabını işlemeden olduğu gibi verir. JSON'u okumak, kontrol etmek ve
hata olursa tekrar istemek onu çağıran kodun işi. Claude da mock da aynı kontrolden geçer.

- **Neden:** En çok bozuk cevapları test etmek istiyorum (yarım JSON, sayı yerine `"30"`,
  fazla alan). İstemci hazır struct verseydi mock bunları üretemezdi ve düzeltme yolu sadece
  gerçek API ile denenirdi. Ham cevap `llm_calls` tablosuna da olduğu gibi yazılıyor.
- **Bedeli:** Tipli sonuç istemcide değil, bir üst katmanda (doğrulayıcıda) elde ediliyor.

### Varsayılan model: Sonnet 5.5

Aynı 6 brief'i Sonnet 5.5 ve Opus 5.5 ile denedim (örnek brief, olmayan şehir, yaş/cinsiyet
gibi desteklenmeyen kriterler, SMS'i uzatan brief vb.). İkisi de 5/5 brief'te ilk
cevapta geçerli sonuç verdi. Fark hızda ve maliyette:

| | Sonnet 5.5 | Opus 5.5 |
|---|---|---|
| Mesaj adımı süresi | 2,8–4,8 sn | 5,8–15,8 sn |
| 6 brief'in maliyeti | ~0,05 $ | ~0,16 $ |

Pazarlamacı ekranda beklediği için hızlı olan Sonnet'i seçtim. Model bir ayar
(`ANTHROPIC_MODEL`), değiştirmek için kod gerekmez. Sınır: sadece 6 brief denedim ve
düzeltme yolu gerçek modelde hiç tetiklenmedi, onu mock testleri kapsıyor.

### `tool_choice` yerine structured outputs

İlk planım tool'u zorla çağırtmaktı ama Opus 5.5 bunu 400 ile reddediyor. Structured outputs
zaten "bana sadece JSON ver" için var, onu kullandım. Şema sadece şekli garanti ediyor;
karakter sınırlarını ve 1–365 gün aralığını şema desteklemediği için kendi kontrolümüz yapıyor.

### Şehir serbest metin, desteklenmeyen kriter bildirilir

Şehir enum olsaydı "Paris" brief'i zorla bilinen bir şehre çevrilirdi ve kimse fark etmeden
yanlış kitleye giderdi. Bu yüzden şehir serbest metin, kontrolü doğrulayıcı yapıyor. Modelin
kural yapamadığı şeyler (yaş, sipariş tutarı, veride olmayan şehir) `unsupported_criteria`
listesine yazılıyor.

Liste doluysa pazarlamacı bunu onaylamadan mesaj adımına geçemiyor. Bunu API de zorluyor,
sadece arayüz değil. Çünkü atılan kriter kitleyi daraltmaz, genişletir: "Paris'teki sepet
terk edenler" tüm şehirlerdeki sepet terk edenlere dönüşür.

### Düzeltme turları

Cevap geçersizse Claude'a orijinal isteği, önceki cevabı ve hataları birlikte gönderiyorum.
Önceki turu yeniden oynatmıyorum, çünkü modellerin düşünme blokları o tura bağlı ve bu
kırılgan. Her adım için en fazla 2 düzeltme ve 2 bağlantı tekrarı var, yani en fazla 5
çağrı. Çağrı başına bütçe verseydim 9 çağrıya çıkardı ve kullanıcı çok beklerdi.
`retry-after` 10 saniyeden uzunsa beklemeden "Claude meşgul" diyorum.

### Düzeltme yetmezse taslak pazarlamacıya kalır

Düzeltmelerden sonra mesaj hâlâ geçersizse (genelde 8 karakter uzun bir SMS), taslak
hatalı alanlar işaretli olarak düzenlemeye açılıyor. Onay butonu geçerli olana kadar kapalı,
onay endpoint'i de aynı kontrolü yapıyor. Pazarlamacı zaten süreçte, 8 karakter kısaltmak
baştan başlamaktan hızlı. Kitle adımında böyle bir yol yok: kitle kuralını elle düzenlemek
beklenmiyor, brief değiştirilir.

## Kitle

### "Son 30 gün" İstanbul saatiyle takvim günü sayılır

Bugün 0, dün 1. 7 Kasım 14:00'de "son 30 gün" 8 Ekim 00:00'da (İstanbul) başlar. Pazarlamacı
bunu kastediyor; "şu andan 30×24 saat önce" kitleyi kampanyanın hazırlandığı saate
bağlardı. Sınırları elle yerleştirdiğim olaylarla test ettim (00:00 ile bir önceki
mikrosaniye, UTC'de ertesi gün olan tarih vb.).

### Referans zaman önizleme ile onay arasında sabit

Önizlemedeki zamanı saklayıp onayda aynen kullanıyorum. Yoksa gece yarısını geçerken
penceredeki ilk günün kullanıcıları bir anda düşerdi. Sadece zaman sabit, kural onayda
yeniden çalışıyor; böylece arada izni geri çeken biri atlanıyor. Bu yüzden onaydaki sayı
önizlemeden biraz farklı olabilir, onay cevabı gerçek sayıyı gösteriyor.

## İçerik

### Karakterler Unicode olarak sayılır

SMS ≤ 160 ve e-posta konusu ≤ 80, bayt değil karakter olarak sayılır ("ş" bir karakter).
GSM-7/UCS-2 (Türkçe karakterde gerçek limitin 70'e düşmesi) kapsam dışı, case'teki 160
kuralını olduğu gibi uyguladım.

### Uzunluk en uzun isme göre kontrol edilir

Her `{{first_name}}` kitledeki en uzun ilk isim kadar sayılıyor. Böylece kontrolü geçen
şablon herkese sığıyor. LLM'ler özellikle Türkçede karakter saymakta zayıf, bu kontrol en
sık takılan yer. Yine de gevşetmedim: prompt limitten daha kısa hedef istiyor ve düzeltme
mesajı tam olarak kaç karakter kısaltılacağını söylüyor.

### Tek değişken `{{first_name}}`, başka süslü parantez yok

Tanımsız değişkenler (`{{name}}`) ve kalan her süslü parantez (`{first_name}`,
`Kod: {SEPET30}` dahil) reddediliyor. Neden: masum bir metni reddetmek pazarlamacıya birkaç
saniye kaybettirir. Bozuk bir yer tutucuyu geçirmek ise yüzlerce kişiye "Merhaba
{first_name}" gönderir ve geri alınamaz. Modelin taslağı ve pazarlamacının elle yazdığı
metin aynı kontrolden geçiyor.

## Gönderim

### Limiti aşan alıcılar için önizleme yapmadım

Taslakta, limiti aşan alıcıları gösteren bir önizleme vardı. Ama doğrulayıcı zaten en uzun
isme göre kontrol ettiği için geçerli bir metinde kimse limiti aşmıyor; önizleme kimseyi
göstermezdi. Bıraktım. Bedeli: bir isim çok uzunsa herkesin bütçesi daralıyor. Veri sonradan
değişirse diye kuyruğa alırken bir güvenlik kontrolü var: limiti aşan SMS isimsiz gider.

### İsim boşsa yer tutucu çıkarılır

`"Merhaba {{first_name}}, sepetin…"` → `"Merhaba, sepetin…"`. "Değerli müşterimiz" gibi bir
kelime yapay duruyor, Claude'dan ayrıca isimsiz sürüm istemek de şemayı ve arayüzü
ikiye katlıyor. Zayıf nokta: `"Sevgili {{first_name}}!"` → `"Sevgili!"`. Bunu kabul ettim.

### Her alıcıya kendi dilinde mesaj

Pazarlamacı brief'i hangi dilde yazarsa yazsın, alıcıya `users.language` dilinde (Türkçe ya
da İngilizce) mesaj gider. Her dil için ayrı taslak var, ama sadece kitlede o dilde alıcı
varsa. Tüm diller tek çağrıda yazılıyor, çeviri değil: böylece teklif ve ton aynı kalıyor,
tek akış ve tek düzeltme döngüsü yetiyor. Revize ederken hangi dillerin yeniden yazılacağı
seçiliyor, diğerleri olduğu gibi kalıyor. Böylece İngilizceyi düzeltmek, kontrol edilmiş
Türkçeyi bozmuyor. Onayda bir alıcının dilinde metin yoksa onay reddediliyor.

### Mesaj metni Go'da üretilir

Tek bir SQL `INSERT … SELECT` düşünmüştüm. Ama boş isim kuralı (büyük harf, noktalama) SQL'de
ikinci bir kopya olurdu ve Go testleri onu kapsamazdı. Alıcıları okuyup Go'da üretiyorum,
hepsi onay transaction'ının içinde.

### Onay kuralları

- Kitle eskiyse (önceki bir günde sayılmışsa) önce yenilenmeli. Yenileme Claude'u çağırmaz.
- Elle yazılmış metin, üretilmiş taslak olmadan da onaylanabilir. Kontrol yine aynı.
- Ulaşılabilir kimse yoksa onay `422` ile reddedilir.
- Cevap, kanal başına kaç mesaj kuyruğa girdiğini söyler.

### Mock sağlayıcı: senkron, hataları ayarlanabilir

Sağlayıcı her isteğe hemen cevap veriyor, webhook yok. Webhook daha gerçekçi olurdu ama
case'in göstermemi istediği şeyi (tekrar deneme, hız limiti, çift gönderim olmaması)
değiştirmeden iş ekler. Hata oranları (5xx, 429, timeout, gecikme) ayarlanabilir ve
varsayılanı sıfır değil, yani her çalıştırmada tekrar deneme yolu denenir. `GET /deliveries`
neyin kaç kez teslim edildiğini gösteriyor; çift gönderim olmadığının kanıtı bu.

### 429'lar rastgele değil, pencereler halinde gelir

İlk planım her istekte %5 ihtimalle 429 dönmekti. 100 mesaj/sn'de bu saniyede 5 429 eder ve
kanal neredeyse hiç göndermezdi. Rastgele 429'lara farklı davranmak sorunu saklar, çünkü
gerçek bir istemci sebebi bilemez. Bu yüzden sağlayıcı yük altındaki gerçek servisler gibi
dönemler halinde kısıtlıyor (ortalama 30 sn'de bir, 2 sn). Worker her 429'a aynı tepkiyi
veriyor: mesajı `Retry-After` kadar erteler, kanalı durdurur, denemeyi saymaz.

### Hız limiti iki tarafta da aynı

Worker ve sağlayıcı kanal başına 100/sn token bucket kullanıyor. Sağlayıcıda sabit saniyelik
pencere olsaydı, 100/sn'ye uyan bir istemci bile bazen 429 görürdü. Artık kurala uyan
istemci sağlayıcının limitine takılmıyor.

### Neyin "deneme" sayıldığı

Mesaj başına en fazla 5 deneme. 429 ve reddedilen bağlantı sayılmıyor, çünkü ikisi de
mesaj hakkında bir şey söylemiyor. Yoksa 20 saniyelik bir kesinti tüm kampanyayı başarısız
yapardı. 5xx, timeout ve `409 in_progress` sayılıyor, çünkü istek sağlayıcıya ulaşmış
olabilir. Bedeli: sağlayıcı kısıtladığı sürece bir mesaj `retry_scheduled`'da bekleyebilir.

### Kanal başına ayrı döngü, korumalı sonuç yazımı

Her kanalın kendi alma sorgusu, goroutine'leri ve limiter'ı var. Ortak olsaydı bir kanalın
birikmesi diğerini tutardı. Sonucu yazarken `status = 'sending' AND attempts = <alınan>`
kontrol ediyorum. Böylece lease süresi dolup satır başkası tarafından alındıktan sonra gelen
eski sonuç, yeni sonucun üstüne yazılmıyor.

### Sağlayıcıda idempotency: sadece başarılar hatırlanır

Sağlayıcı yeni `message_id`'yi "işleniyor" yapar, başarıda "gönderildi" olarak tutar,
hatada unutur. Yoksa önbelleğe alınmış bir 5xx sonraki tüm denemeleri engellerdi. Gönderilmiş
bir id tekrar gelirse `duplicate: true` döner. Bellekte tutuluyor: sağlayıcı yeniden
başlarsa unutur, mock için kabul ettim.

### Timeout: yarısı teslimden önce, yarısı sonra

Timeout'ların yarısı mesaj işlenmeden takılır (sıradan tekrar), yarısı teslim edildikten
sonra. İkinci durumda worker teslim edilmiş bir mesajı tekrar dener ve sağlayıcı
`duplicate` dönmelidir. Idempotency anahtarı tam bunun için var, o yüzden mock bunun
olmasını garanti ediyor.

### Kalıcı hatalar sabit alıcılardan gelir

Türk cep numarası olmayan telefonlar ve `.invalid` biten e-postalar `400` ile reddedilir ve
seed'de bunlardan birer kullanıcı var. Rastgele 400 olsaydı kalıcı hata yolu tesadüfen
görünürdü; böyle her demoda ve testte görünüyor.

### Reddedilen alıcılar kampanya sayfasında listelenir

Başarısız mesaj nedenini `messages.failure` sütununda tutar: `permanent` (sağlayıcı
reddetti) ya da `retries_exhausted`. Sadece kalıcı olanlar kampanya sayfasında ve CSV'de
çıkar, çünkü bu alıcıların verisi bozuk ve biri düzeltmeli. Tükenen tekrar alıcı hakkında
bir şey söylemiyor. Liste loglardan değil `messages` tablosundan geliyor; böylece log silinse
bile kalıyor ve adresler loglara girmiyor. CSV Türkçe Excel için: BOM'lu UTF-8, `;` ayırıcı,
ve `=`, `+`, `-`, `@` ile başlayan değerlerin önünde `'`.

### Zamanlanmış gönderim: onayda kuyruğa alınır

Alıcılar ve metinler gönderim anında değil onay anında oluşturuluyor. Böylece giden şey
pazarlamacının gördüğü şey oluyor ve ayrı bir zamanlayıcı süreci gerekmiyor. Mesajlar normal
şekilde kuyruğa giriyor, sadece `next_attempt_at` seçilen zaman. Kampanyanın ayrı bir
`scheduled` durumu var; worker ilk mesajı alınca onu `approved` yapıyor. İptal edilirse
bekleyen mesajlar silinir ve kampanya `ready`'ye döner. Gönderim başlamışsa iptal
`409 send_started` verir.

Zaman kuralları (İstanbul saati): en az 1 dakika, en çok 14 gün sonrası ve 21:00–09:00 arası
olmaz (gece ticari mesaj olmasın diye). Kitle onayda donduğu için 14 gün sınırı kitlenin ne
kadar eskiyebileceğini de sınırlıyor.

### Worker sağlığı

Worker'ın `/healthz`'i sadece veritabanı cevap veriyorsa ve her kanal döngüsü son 10 saniyede
çalıştıysa sağlıklı diyor. Yani takılmış bir döngü de yakalanıyor. Dashboard worker'a
doğrudan ulaşamadığı için `/api/metrics` worker'ın durumunu da içeriyor.

### Backoff'ta eşit jitter

Bekleme d/2 ile d arasında rastgele. Tam jitter neredeyse hemen tekrar edebiliyor, ±%20 ise
neredeyse hiç dağıtmıyor. Hız limiti zaten tekrar dalgasını yumuşatıyor, o yüzden fark küçük
ve tek bir kural olması işimi kolaylaştırıyor.

## İzleme

### Metrikler bir servis kapalıyken de cevap verir

`GET /api/metrics` worker ve sağlayıcının durumunu canlı çekiyor (her biri için 1 sn
timeout). Biri cevap vermezse tüm cevap hata olmuyor, o parça `unreachable` yazıyor.
Dashboard'un işi tam da hangi parçanın kapalı olduğunu göstermek.

### Teslim akışı kampanya bitince kapanır

`/api/campaigns/{id}/events` önce mevcut sayıları, sonra değişince yenilerini gönderir.
Kampanya `completed` olunca `done` ile kapanır. Gönderim hızı son 5 saniyenin ortalaması:
okunabilir ama başlarken ve biterken gerçeğin biraz gerisinde kalıyor.

### Loglar: stdout'a hepsi, Postgres'e sadece uyarı ve hata

Her servis JSON log yazıyor, `docker compose logs` tam kayıt. Uyarı ve hatalar ayrıca
`app_logs` tablosuna gidiyor ve log sayfasında gösteriliyor; böylece sorunu terminal
açmadan bulabilirsin. Loki + Grafana düşündüm ama tek komutla kalkması gereken proje için
2-3 ek container fazla. Dosyaya yazmak rotasyon getirir ve arayüzden aranamaz.

- Sadece warn ve error saklanıyor. Saniyede yüzlerce gönderimde info loglar işe yaramaz
  yük olurdu.
- Kayıtlar tamponlanıp toplu yazılıyor. Tampon dolarsa kayıt atılıyor ve bu stdout'a
  yazılıyor, yani loglama isteği ya da gönderimi yavaşlatmıyor.
- Veritabanı kapalıysa hatalar yine stdout'ta duruyor.
- Kayıtlar `LOG_RETENTION_DAYS` (varsayılan 7) gün sonra siliniyor.

Bir kampanyayı baştan sona izlemek için `campaign_id` kullanıyorum, çünkü API ile worker
arasında kuyruk var ve istek ID'si bunu takip edemez. Kampanyayla ilgili her log satırında
`campaign_id` var. Her istek ayrıca bir request ID alıyor; 500 hatasında arayüz bunu "Hata
kodu" olarak gösteriyor.

Neyin hata sayılacağı önemli: mock sağlayıcı %5 hata veriyor, yani tekrar denemeler
normal ve uyarı. Vazgeçilen mesaj, başarısız LLM adımı ve 500 ise hata.

## Frontend

### Frontend'i API sunar, nginx yok

Dockerfile Vue uygulamasını derleyip imaja koyuyor ve API onu sunuyor. Yani ek container
olmadan `docker compose up` arayüzü `localhost:8080`'de açıyor. Taslakta nginx vardı ama
yerel demo için fazla: ek imaj, ayar ve uzun süren SSE bağlantıları için ekstra bir
atlama demek. Tek origin olunca CORS da yok. Bedeli: frontend değişince API imajını
yeniden derlemek gerekiyor. Geliştirirken Vite arayüzü sunuyor ve `/api`'yi yönlendiriyor.
