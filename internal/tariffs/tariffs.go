// Package tariffs — тарифы пополнения баланса трафика.
// Цены и объёмы проще править здесь, чем в БД. Если захочешь динамические тарифы
// (промокоды, персональные цены) — вынесешь в таблицу позже.
package tariffs

type Tariff struct {
	ID          string  // "basic_10", "plus_30", "pro_100"
	Description string  // показывается в Робокассе: "VPN Panel — 30 ГБ"
	Label       string  // UI-подпись: "30 ГБ"
	Amount      float64 // цена в рублях
	TrafficGB   float64 // сколько ГБ начислится на баланс после оплаты
	Popular     bool
}

// List — доступные тарифы в порядке отображения.
var List = []Tariff{
	{ID: "basic_10", Label: "10 ГБ", Amount: 150, TrafficGB: 10, Description: "VPN Panel — 10 ГБ"},
	{ID: "plus_30", Label: "30 ГБ", Amount: 300, TrafficGB: 30, Description: "VPN Panel — 30 ГБ", Popular: true},
	{ID: "pro_100", Label: "100 ГБ", Amount: 700, TrafficGB: 100, Description: "VPN Panel — 100 ГБ"},
}

func FindByID(id string) *Tariff {
	for i := range List {
		if List[i].ID == id {
			return &List[i]
		}
	}
	return nil
}
