// Variante experimental (-PwithRhino): sustituye el QuickJs de AndroidCompat (que usa GraalJS, ~120 MB de jars)
// por uno sobre Mozilla Rhino (~1,5 MB) en modo intérprete. Va en el jar del host, que precede a AndroidCompat
// en el classpath, así que esta clase gana. Misma API pública que la original de Suwayomi.
package app.cash.quickjs;

import org.mozilla.javascript.Context;
import org.mozilla.javascript.NativeArray;
import org.mozilla.javascript.Scriptable;
import org.mozilla.javascript.ScriptableObject;
import org.mozilla.javascript.Undefined;
import org.mozilla.javascript.Wrapper;

import java.io.Closeable;

public final class QuickJs implements Closeable {
    private final Context cx;
    private final Scriptable scope;

    public static QuickJs create() {
        return new QuickJs();
    }

    public QuickJs() {
        cx = Context.enter();
        cx.setInterpretedMode(true); // sin generar bytecode: menos memoria y arranque inmediato
        cx.setLanguageVersion(Context.VERSION_ECMASCRIPT);
        scope = cx.initSafeStandardObjects();
    }

    public Object evaluate(String script, String ignoredFileName) {
        return evaluate(script);
    }

    public Object evaluate(String script) {
        try {
            return translate(cx.evaluateString(scope, script, "script", 1, null));
        } catch (Exception e) {
            throw new QuickJsException(e.getMessage(), e);
        }
    }

    private Object translate(Object v) {
        if (v == null || v instanceof Undefined) return null;
        if (v instanceof Wrapper) return ((Wrapper) v).unwrap();
        if (v instanceof Boolean) return v;
        if (v instanceof CharSequence) return v.toString();
        if (v instanceof Number) return number((Number) v);
        if (v instanceof NativeArray) {
            NativeArray a = (NativeArray) v;
            int n = (int) a.getLength();
            if (n == 0) return new int[0];
            Object first = a.get(0, a);
            if (first instanceof Boolean) {
                boolean[] out = new boolean[n];
                for (int i = 0; i < n; i++) out[i] = Boolean.TRUE.equals(a.get(i, a));
                return out;
            } else if (first instanceof Number) {
                boolean ints = true;
                double[] d = new double[n];
                for (int i = 0; i < n; i++) {
                    d[i] = ((Number) a.get(i, a)).doubleValue();
                    if (d[i] != Math.rint(d[i]) || Math.abs(d[i]) > Integer.MAX_VALUE) ints = false;
                }
                if (!ints) return d;
                int[] out = new int[n];
                for (int i = 0; i < n; i++) out[i] = (int) d[i];
                return out;
            } else if (first instanceof CharSequence) {
                String[] out = new String[n];
                for (int i = 0; i < n; i++) out[i] = String.valueOf(a.get(i, a));
                return out;
            }
            Object[] out = new Object[n];
            for (int i = 0; i < n; i++) out[i] = translate(a.get(i, a));
            return out;
        }
        return v;
    }

    private static Object number(Number n) {
        double d = n.doubleValue();
        if (d == Math.rint(d) && !Double.isInfinite(d)) {
            if (Math.abs(d) <= Integer.MAX_VALUE) return (int) d;
            if (Math.abs(d) < 9.0e18) return (long) d;
        }
        return d;
    }

    public byte[] compile(String sourceCode, String ignoredFileName) {
        return sourceCode.getBytes();
    }

    public Object execute(byte[] bytecode) {
        return evaluate(new String(bytecode));
    }

    public <T> void set(String name, Class<T> ignoredType, T object) {
        ScriptableObject.putProperty(scope, name, Context.javaToJS(object, scope));
    }

    @Override
    public void close() {
        Context.exit();
    }
}
