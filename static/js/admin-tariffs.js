// Копирование тарифа: берём поля из формы редактирования, переносим в форму
// создания сверху. Код обязательно меняем (unique-constraint в БД), чтобы юзер
// сразу видел, что его нужно отредактировать.
function copyTariff(btn) {
    const src = btn.closest('form');
    const dst = document.getElementById('new-tariff-form');
    if (!src || !dst) return;

    const names = ['kind', 'code', 'label', 'description',
                   'amount_rub', 'traffic_gb', 'duration_days', 'sort_order'];
    for (const name of names) {
        const s = src.elements.namedItem(name);
        const d = dst.elements.namedItem(name);
        if (!s || !d) continue;
        d.value = name === 'code' ? s.value + '_copy' : s.value;
    }
    for (const name of ['is_popular', 'is_active']) {
        const s = src.elements.namedItem(name);
        const d = dst.elements.namedItem(name);
        if (s && d) d.checked = s.checked;
    }

    dst.scrollIntoView({behavior: 'smooth', block: 'start'});
    const codeField = dst.elements.namedItem('code');
    if (codeField) {
        codeField.focus();
        codeField.select();
    }
}
