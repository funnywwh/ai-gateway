// Money rendering for the console (M22).
//
// Two kinds of amounts exist and they must never be confused:
//   * ledger amounts — balances, credit, ledger entries, invoices. They are kept in
//     the ledger currency and are shown in whichever currency the operator picked;
//     a converted number is marked with "≈" so it is never mistaken for the amount
//     that was actually booked.
//   * native amounts — model prices and simulator results. They belong to the
//     currency their rule set declares and are shown as they are, with the code.
//
// The conversion here is display-only: it never writes anything back, which is why
// it can live in the browser. It still uses BigInt because a micro amount times a
// rate overflows the exact range of a JavaScript number.

import { api } from './api.js';

const STORAGE_KEY = 'aigw.display_currency';
const MICRO = 1_000_000n;

let table = null;
let loading = null;

// initCurrency loads the currency table once per page load. Pages and the topbar
// selector both call it, so the in-flight request is shared rather than duplicated.
// A failure surfaces where the page can show it and is not cached.
export async function initCurrency() {
  if (table) return table;
  if (!loading) {
    loading = api.get('/billing/currency').then((payload) => {
      const currencies = payload.currencies || [];
      table = {
        ledger: payload.ledger_currency || 'USD',
        preferred: payload.display_currency || payload.ledger_currency || 'USD',
        fxSource: payload.fx_source || 'config',
        missing: payload.missing_rates || [],
        currencies,
        rates: new Map(currencies.map((entry) => [entry.code, Number(entry.rate_micros)])),
      };
      return table;
    }).catch((err) => {
      loading = null;
      throw err;
    });
  }
  return loading;
}

export function currencyTable() { return table; }

export function ledgerCurrency() { return table ? table.ledger : 'USD'; }

export function currencies() { return table ? table.currencies : []; }

export function missingRates() { return table && table.missing ? table.missing : []; }

export function fxSource() { return table ? table.fxSource : 'config'; }

// displayCurrency resolves the operator's choice: the stored one when it is still
// convertible, otherwise the server-side default.
export function displayCurrency() {
  const codes = currencies().map((entry) => entry.code);
  let stored = null;
  try { stored = window.localStorage.getItem(STORAGE_KEY); } catch (err) { stored = null; }
  if (stored && codes.includes(stored)) return stored;
  return table ? table.preferred : 'USD';
}

export function setDisplayCurrency(code) {
  try { window.localStorage.setItem(STORAGE_KEY, code); } catch (err) { /* private mode: keep it for this page only */ }
  window.dispatchEvent(new CustomEvent('aigw:currency', { detail: { code } }));
}

// convertToDisplay moves a ledger amount into the display currency, rounding to
// nearest exactly like the server does for its own display conversions.
function convertToDisplay(micros, code) {
  const ledger = ledgerCurrency();
  if (!table || code === ledger) return BigInt(micros || 0);
  const rate = table.rates.get(code);
  const ledgerRate = table.rates.get(ledger);
  if (!rate || !ledgerRate) return null;
  const value = BigInt(micros || 0) * BigInt(ledgerRate);
  const divisor = BigInt(rate);
  return (value + divisor / 2n) / divisor;
}

const format = (micros) => {
  const negative = micros < 0n;
  const absolute = negative ? -micros : micros;
  const whole = absolute / MICRO;
  const fraction = (absolute % MICRO).toString().padStart(6, '0');
  return (negative ? '-' : '') + whole.toString() + '.' + fraction;
};

function codeOf(code) { return code || displayCurrency(); }

// money renders a ledger amount (balance, ledger entry, invoice, aggregate) in the
// chosen display currency. A converted value carries "≈" and the code; the exact
// one carries the code alone, so a reader can always tell them apart.
export function money(micros) {
  const code = displayCurrency();
  const converted = convertToDisplay(micros, code);
  if (converted === null) return '— ' + code;
  const prefix = code === ledgerCurrency() ? '' : '≈';
  return prefix + format(converted) + ' ' + code;
}

// moneyExact renders a ledger amount without any conversion (the number that was
// actually booked), used where a converted value would be misleading.
export function moneyExact(micros, code) {
  return format(BigInt(micros || 0)) + ' ' + codeOf(code);
}

// nativeMoney renders an amount that already belongs to a model's own currency:
// no conversion, always labelled.
export function nativeMoney(micros, code) {
  return format(BigInt(micros || 0)) + ' ' + (code || ledgerCurrency());
}

// microsPerUnit explains a rate the way an operator writes it: 1 CNY = 0.141000 USD.
export function describeRate(code) {
  const rate = table ? table.rates.get(code) : null;
  if (!rate) return '—';
  return '1 ' + code + ' = ' + format(BigInt(rate)) + ' ' + ledgerCurrency();
}
