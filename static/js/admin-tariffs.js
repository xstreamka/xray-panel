// Копирование тарифа: берём поля из формы редактирования, переносим в форму
// создания сверху. Код обязательно меняем (unique-constraint в БД), чтобы юзер
// сразу видел, что его нужно отредактировать.
// Inline onclick недопустим из-за CSP (script-src 'self' без unsafe-inline) —
// навешиваем обработчики через делегирование на data-атрибуты.
function copyTariff(btn) {
    const src = btn.closest('form');
    const dst = document.getElementById('new-tariff-form');
    if (!src || !dst) return;

    const names = ['kind', 'code', 'label', 'description',
                   'amount_rub', 'traffic_gb', 'duration_days', 'sort_order',
                   'discount_percent'];
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

document.addEventListener('click', (e) => {
    const copyBtn = e.target.closest('[data-copy-tariff]');
    if (copyBtn) {
        copyTariff(copyBtn);
        return;
    }
    const delBtn = e.target.closest('[data-delete-tariff]');
    if (delBtn) {
        const code = delBtn.dataset.tariffCode || '';
        if (!confirm(`Удалить тариф ${code}? Это необратимо.`)) return;
        const form = document.getElementById(delBtn.dataset.deleteTariff);
        if (form) form.submit();
    }
});
