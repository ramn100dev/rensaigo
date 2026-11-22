package android.icu.text;

import java.util.Comparator;
import java.util.Locale;

/**
 * Sustituto mínimo de android.icu.text.Collator (en android.jar solo hay stubs que lanzan "Stub!"). Se apoya en
 * java.text.Collator. Lo usan extensiones que ordenan títulos o capítulos como el navegador (p. ej. Aquellos Que
 * Traducen: localeCompare con numeric:true). El jar del host va antes que android-jar en el classpath.
 */
public abstract class Collator implements Comparator<Object>, Cloneable {
    public static final int PRIMARY = 0;
    public static final int SECONDARY = 1;
    public static final int TERTIARY = 2;
    public static final int QUATERNARY = 3;
    public static final int IDENTICAL = 15;

    protected boolean frozen;

    public static Collator getInstance() { return getInstance(Locale.getDefault()); }

    public static Collator getInstance(Locale locale) { return new RuleBasedCollator(locale); }

    public abstract int compare(String a, String b);

    @Override
    public int compare(Object a, Object b) { return compare(String.valueOf(a), String.valueOf(b)); }

    public boolean equals(String a, String b) { return compare(a, b) == 0; }

    public abstract int getStrength();

    public abstract void setStrength(int strength);

    public Collator freeze() { frozen = true; return this; }

    public boolean isFrozen() { return frozen; }

    public Collator cloneAsThawed() {
        try {
            Collator c = (Collator) super.clone();
            c.frozen = false;
            return c;
        } catch (CloneNotSupportedException e) {
            throw new IllegalStateException(e);
        }
    }

    @Override
    public Object clone() throws CloneNotSupportedException { return super.clone(); }

    protected void checkNotFrozen() {
        if (frozen) throw new UnsupportedOperationException("Collator congelado");
    }
}
