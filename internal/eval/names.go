package eval

// friendly run-name words + generator: a 31x string hash seeds an xorshift32,
// which picks adjective + noun. same seed -> same name, so a run's words are a
// pure function of its numeric prefix.

var nameAdj = []string{
	"angora", "argyle", "bobbled", "braided", "brioche", "brushed", "bulky", "cabled",
	"cashmere", "chevron", "chunky", "circular", "colorwork", "cozy", "crocheted",
	"crosshatched", "darned", "delicate", "double-knit", "drapey", "dyedinthewool",
	"embossed", "entrelac", "fair-isle", "felted", "fingering", "fleece", "fluffy",
	"frogged", "fuzzy", "garter", "gauzy", "gradient", "handspun", "heathered",
	"herringbone", "houndstooth", "intarsia", "jacquard", "laceweight", "lacy",
	"loopy", "marled", "mercerized", "merino", "mohair", "mosaic", "nubby",
	"openwork", "plied", "puckered", "purled", "quilted", "ribbed", "rippled",
	"roving", "ruffled", "seamless", "seed-stitch", "selvage", "shetland", "silky",
	"slipped", "squishy", "stockinette", "striped", "superwash", "tabby", "tangled",
	"tapestry", "tasseled", "textured", "thick", "thrummed", "tufted", "twisted",
	"undyed", "variegated", "velvet", "waffle", "warp-knit", "washable", "weft",
	"woolly", "worsted", "woven", "yarn-over",
}

var nameNoun = []string{
	"armwarmer", "balaclava", "beanie", "blanket", "bobbin", "bolero", "bonnet",
	"bootie", "bra", "bralette", "brioche", "buttonhole", "cable", "cardigan",
	"cast-on", "chevron", "cloche", "coaster", "collar", "cowl", "cuff", "cushion",
	"dishcloth", "doily", "drop-stitch", "earmuff", "edging", "eyelet", "fair-isle",
	"fringe", "garter", "gauge", "glove", "granny-square", "gusset", "hat",
	"headband", "hoodie", "intarsia", "jumper", "kerchief", "lace", "lanyard",
	"legwarmer", "loop", "mitten", "motif", "muffler", "neckwarmer", "needle",
	"patchwork", "pattern", "picot", "pillowcase", "placemat", "pocket", "pompom",
	"poncho", "pouch", "pullover", "purse", "raglan", "ribbons", "runner", "scarf",
	"shawl", "shrug", "singlet", "skein", "sleeve", "slipper", "snood", "sock",
	"spool", "stitch", "stocking", "stole", "sweater", "swatch", "tam", "tassel",
	"tea-cozy", "tension", "throw", "toque", "tunic", "turban", "vest", "washcloth",
	"wristlet", "yoke",
}

// generateName picks "<adj>-<noun>" deterministically from seed, mirroring the
// js original: h = 31*h + charCode over the seed (int32 wrap), then xorshift32
// per draw, mapped to [0,1) via the unsigned value.
func generateName(seed string) string {
	var h int32
	for i := 0; i < len(seed); i++ {
		h = 31*h + int32(seed[i])
	}
	rand := func() float64 {
		h ^= h << 13
		h ^= h >> 17 // NOTE: arithmetic shift, matching js >> on int32
		h ^= h << 5
		return float64(uint32(h)) / 4294967296
	}
	a := nameAdj[int(rand()*float64(len(nameAdj)))]
	n := nameNoun[int(rand()*float64(len(nameNoun)))]
	return a + "-" + n
}
