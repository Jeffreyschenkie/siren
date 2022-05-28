package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"text/template"

	"gopkg.in/yaml.v3"
)

// ParseKind specifies Telegram message parsing method
//go:generate yamlenums -type=ParseKind
type ParseKind int

const (
	// ParseRaw parses Telegram message as a raw text
	ParseRaw ParseKind = iota
	// ParseHTML parses Telegram message as HTML
	ParseHTML
	// ParseMarkdown parses Telegram message as Markdown
	ParseMarkdown
)

func (r ParseKind) String() string {
	switch r {
	case ParseRaw:
		return "raw"
	case ParseHTML:
		return "html"
	case ParseMarkdown:
		return "markdown"
	}
	return "unknown"
}

// Translation represents a translated text for a Telegram message
type Translation struct {
	Key            string    `yaml:"-"`
	Str            string    `yaml:"str"`
	Parse          ParseKind `yaml:"parse"`
	DisablePreview bool      `yaml:"disable_preview"`
	Image          string    `yaml:"image"`
	ImageBytes     []byte    `yaml:"-"`
}

// AllTranslations represents a collection of translated texts in all supported languages
type AllTranslations map[string]*Translation

// Translations represents a collection of translated texts for Telegram messages
type Translations struct {
	Help                        *Translation `yaml:"help"`
	Online                      *Translation `yaml:"online"`
	List                        *Translation `yaml:"list"`
	Offline                     *Translation `yaml:"offline"`
	Denied                      *Translation `yaml:"denied"`
	SyntaxAdd                   *Translation `yaml:"syntax_add"`
	SyntaxRemove                *Translation `yaml:"syntax_remove"`
	SyntaxFeedback              *Translation `yaml:"syntax_feedback"`
	InvalidSymbols              *Translation `yaml:"invalid_symbols"`
	AlreadyAdded                *Translation `yaml:"already_added"`
	AddError                    *Translation `yaml:"add_error"`
	ModelAdded                  *Translation `yaml:"model_added"`
	ModelNotInList              *Translation `yaml:"model_not_in_list"`
	ModelRemoved                *Translation `yaml:"model_removed"`
	CheckingModel               *Translation `yaml:"checking_model"`
	Feedback                    *Translation `yaml:"feedback"`
	Social                      *Translation `yaml:"social"`
	UnknownCommand              *Translation `yaml:"unknown_command"`
	InvalidCommand              *Translation `yaml:"invalid_command"`
	Languages                   *Translation `yaml:"languages"`
	Version                     *Translation `yaml:"version"`
	ProfileRemoved              *Translation `yaml:"profile_removed"`
	NoOnlineModels              *Translation `yaml:"no_online_models"`
	RemoveAll                   *Translation `yaml:"remove_all"`
	AllModelsRemoved            *Translation `yaml:"all_models_removed"`
	ReferralLink                *Translation `yaml:"referral_link"`
	InvalidReferralLink         *Translation `yaml:"invalid_referral_link"`
	FollowerExists              *Translation `yaml:"follower_exists"`
	ReferralApplied             *Translation `yaml:"referral_applied"`
	OwnReferralLinkHit          *Translation `yaml:"own_referral_link_hit"`
	SubscriptionUsage           *Translation `yaml:"subscription_usage"`
	SubscriptionUsageAd         *Translation `yaml:"subscription_usage_ad"`
	NotEnoughSubscriptions      *Translation `yaml:"not_enough_subscriptions"`
	Week                        *Translation `yaml:"week"`
	ZeroSubscriptions           *Translation `yaml:"zero_subscriptions"`
	FAQ                         *Translation `yaml:"faq"`
	RawCommands                 *Translation `yaml:"raw_commands"`
	Settings                    *Translation `yaml:"settings"`
	OK                          *Translation `yaml:"ok"`
	TooManySubscriptionsForPics *Translation `yaml:"too_many_subscriptions_for_pics"`
	WeAreUp                     *Translation `yaml:"we_are_up"`
}

// LoadEndpointTranslations loads translations for a specific endpoint
func LoadEndpointTranslations(files []string) (*Translations, AllTranslations) {
	tr := &Translations{}
	allTr := AllTranslations{}
	for _, t := range files {
		parsed := loadTranslations(t)
		for k, v := range parsed {
			v.Key = k
			allTr[k] = v
		}
	}
	copyTranslations(allTr, tr)
	CheckErr(noNils(tr))
	return tr, allTr
}

// LoadAllTranslations loads all translations
func LoadAllTranslations(files map[string][]string) (trMap map[string]*Translations, tpl map[string]*template.Template) {
	trMap = make(map[string]*Translations)
	tpl = make(map[string]*template.Template)
	for endpoint, x := range files {
		tr, allTr := LoadEndpointTranslations(x)
		trMap[endpoint] = tr
		tpl[endpoint] = setupTemplates(allTr)
	}
	return
}

// LoadAds loads ads for a specific endpoint
func LoadAds(files []string) AllTranslations {
	allTr := AllTranslations{}
	for _, t := range files {
		parsed := loadTranslations(t)
		for k, v := range parsed {
			v.Key = k
			allTr[k] = v
		}
	}
	return allTr
}

// LoadAllAds loads all ads
func LoadAllAds(files map[string][]string) (trMap map[string]map[string]*Translation, tpl map[string]*template.Template) {
	trMap = make(map[string]map[string]*Translation)
	tpl = make(map[string]*template.Template)
	for endpoint, x := range files {
		allTr := LoadAds(x)
		trMap[endpoint] = allTr
		tpl[endpoint] = setupTemplates(allTr)
	}
	return
}

// ToMap returns translations as a map
func (x *Translations) ToMap() map[string]*Translation {
	rv := reflect.ValueOf(x).Elem()
	result := map[string]*Translation{}
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		tag := rv.Type().Field(i).Tag.Get("yaml")
		result[tag] = field.Interface().(*Translation)
	}
	return result
}

func setupTemplates(trs AllTranslations) *template.Template {
	tpl := template.New("")
	tpl.Funcs(template.FuncMap{"mod": func(i, j int) int { return i % j }})
	tpl.Funcs(template.FuncMap{"add": func(i, j int) int { return i + j }})
	for k, v := range trs {
		template.Must(tpl.New(k).Parse(v.Str))
	}

	return tpl
}

func copyTranslations(from AllTranslations, to *Translations) {
	value := reflect.ValueOf(to).Elem()
	toType := reflect.TypeOf(to).Elem()
	for k, v := range from {
		for i := 0; i < value.NumField(); i++ {
			tag := toType.Field(i).Tag.Get("yaml")
			if tag == k {
				f := value.Field(i)
				f.Set(reflect.ValueOf(v))
				continue
			}
		}
	}
}

func noNils(x *Translations) error {
	rv := reflect.ValueOf(x).Elem()
	for i := 0; i < rv.NumField(); i++ {
		field := rv.Field(i)
		if field.IsNil() {
			tag := rv.Type().Field(i).Tag.Get("yaml")
			return fmt.Errorf("required translation is not set: %s", tag)
		}
	}
	return nil
}

func loadTranslations(path string) AllTranslations {
	file, err := os.Open(filepath.Clean(path))
	CheckErr(err)
	defer func() { CheckErr(file.Close()) }()
	decoder := yaml.NewDecoder(file)
	parsed := AllTranslations{}
	err = decoder.Decode(&parsed)
	CheckErr(err)
	return parsed
}
