package middleware

import "net/http"

// MaxBodyBytes ограничивает размер тела запроса для всех write-методов.
// Лимит применяется ДО ParseForm/FormValue: иначе r.FormValue утянет в память
// сколько бы клиент ни прислал, а уже потом мы проверим длину — DoS-вектор.
// 64 KiB с запасом покрывает все реальные формы панели; feedback ставит
// собственный, более жёсткий MaxBytesReader локально и поверх перезаписывает.
func MaxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
