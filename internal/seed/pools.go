package seed

// Hand-curated pools: common Turkish names and the most populous cities. Turkish
// characters (ş, ğ, ı, İ, ç, ö, ü) are intentional — they exercise rune counting,
// city normalization and rendered-length checks with realistic data.

var firstNames = []string{
	"Ayşe", "Fatma", "Emine", "Hatice", "Zeynep", "Elif", "Merve", "Büşra", "Esra", "Özge",
	"Şeyma", "Gülşen", "Derya", "Ebru", "Sibel", "Tuğba", "Gizem", "İrem", "Ece", "Defne",
	"Nur", "Yağmur", "Damla", "Çağla", "Sevgi", "Hülya", "Melek", "Öykü", "Aslı", "Pınar",
	"Mehmet", "Mustafa", "Ahmet", "Ali", "Hüseyin", "Hasan", "İbrahim", "İsmail", "Osman", "Yusuf",
	"Murat", "Ömer", "Emre", "Burak", "Can", "Cem", "Barış", "Oğuz", "Serkan", "Uğur",
	"Tolga", "Kaan", "Eren", "Arda", "Çağrı", "Gökhan", "Şükrü", "Doğan", "Erdem", "Volkan",
}

var lastNames = []string{
	"Yılmaz", "Kaya", "Demir", "Şahin", "Çelik", "Yıldız", "Yıldırım", "Öztürk", "Aydın", "Özdemir",
	"Arslan", "Doğan", "Kılıç", "Aslan", "Çetin", "Kara", "Koç", "Kurt", "Özkan", "Şimşek",
	"Polat", "Özçelik", "Korkmaz", "Çakır", "Erdoğan", "Yavuz", "Can", "Acar", "Şen", "Aktaş",
	"Güler", "Yalçın", "Güneş", "Bozkurt", "Bulut", "Keskin", "Ünal", "Turan", "Gül", "Özer",
}

// Weights roughly follow population, so an "İstanbul" segment is sizeable but not everyone.
var cities = []weighted{
	{"İstanbul", 35}, {"Ankara", 15}, {"İzmir", 12}, {"Bursa", 8}, {"Antalya", 7}, {"Adana", 5},
	{"Konya", 5}, {"Gaziantep", 4}, {"Kocaeli", 3}, {"Mersin", 2}, {"Kayseri", 2}, {"Eskişehir", 2},
}

var mobilePrefixes = []string{
	"530", "532", "533", "535", "536", "538", "539", "541", "542", "543",
	"544", "545", "546", "549", "551", "552", "553", "554", "555",
}

// Cities returns the city names the generator uses, i.e. every city in a seeded database.
func Cities() []string {
	out := make([]string, len(cities))
	for i, c := range cities {
		out[i] = c.value
	}
	return out
}
