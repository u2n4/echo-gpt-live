import java.lang.reflect.Method;

/** Reads the active source from the Huawei TV's local TV service. */
public final class HuaweiInput {
    public static void main(String[] args) throws Exception {
        Class<?> binder = Class.forName("android.os.IBinder");
        Object serviceBinder = Class.forName("android.os.ServiceManager")
            .getMethod("getService", String.class).invoke(null, "hwtvservice");
        Class<?> service = Class.forName("com.huawei.tvapi.ITvService");
        Object tv = Class.forName("com.huawei.tvapi.ITvService$Stub")
            .getMethod("asInterface", binder).invoke(null, serviceBinder);
        if (tv == null) {
            throw new IllegalStateException("Huawei TV service is unavailable");
        }
        Object source = service.getMethod("getTvSource").invoke(tv);
        Method current = Class.forName("com.huawei.tvapi.ITvSource")
            .getMethod("getCurSourceId", int.class);
        Method signal = Class.forName("com.huawei.tvapi.ITvSource").getMethod("getSignalStatus");
        System.out.println(current.invoke(source, 0) + " " + signal.invoke(source));
    }
}
