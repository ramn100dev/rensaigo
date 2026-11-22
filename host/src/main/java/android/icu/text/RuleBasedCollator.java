package android.icu.text;

import java.util.Locale;

/** Ver Collator. Añade la ordenación numérica ("cap 2" antes que "cap 10"). */
public final class RuleBasedCollator extends Collator {
    private java.text.Collator base;
    private int strength = TERTIARY;
    private boolean numeric;

    RuleBasedCollator(Locale locale) {
        base = java.text.Collator.getInstance(locale == null ? Locale.ROOT : locale);
        base.setStrength(java.text.Collator.TERTIARY);
        base.setDecomposition(java.text.Collator.CANONICAL_DECOMPOSITION);
    }

    @Override
    public int getStrength() { return strength; }

    @Override
    public void setStrength(int s) {
        checkNotFrozen();
        strength = s;
        base.setStrength(switch (s) {
            case PRIMARY -> java.text.Collator.PRIMARY;
            case SECONDARY -> java.text.Collator.SECONDARY;
            case IDENTICAL -> java.text.Collator.IDENTICAL;
            default -> java.text.Collator.TERTIARY;
        });
    }

    public boolean getNumericCollation() { return numeric; }

    public void setNumericCollation(boolean on) { checkNotFrozen(); numeric = on; }

    @Override
    public int compare(String a, String b) {
        if (!numeric) return base.compare(a, b);
        int i = 0, j = 0;
        while (i < a.length() && j < b.length()) {
            boolean da = Character.isDigit(a.charAt(i)), db = Character.isDigit(b.charAt(j));
            int ei = run(a, i, da), ej = run(b, j, db);
            int c;
            if (da && db) {
                String x = strip(a.substring(i, ei)), y = strip(b.substring(j, ej));
                c = x.length() != y.length() ? Integer.compare(x.length(), y.length()) : x.compareTo(y);
            } else {
                c = base.compare(a.substring(i, ei), b.substring(j, ej));
            }
            if (c != 0) return c;
            i = ei;
            j = ej;
        }
        return Integer.compare(a.length() - i, b.length() - j);
    }

    private static int run(String s, int from, boolean digits) {
        int k = from;
        while (k < s.length() && Character.isDigit(s.charAt(k)) == digits) k++;
        return k;
    }

    private static String strip(String d) {
        int k = 0;
        while (k < d.length() - 1 && d.charAt(k) == '0') k++;
        return d.substring(k);
    }

    @Override
    public Object clone() throws CloneNotSupportedException {
        RuleBasedCollator c = (RuleBasedCollator) super.clone();
        c.base = (java.text.Collator) base.clone();
        return c;
    }
}
