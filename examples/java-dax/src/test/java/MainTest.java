import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import org.junit.jupiter.api.Test;
import static org.junit.jupiter.api.Assertions.*;

class MainTest {
    @Test void remoteApiIsRefused() {
        assertThrows(IllegalArgumentException.class, () -> new Main.Router("http://192.0.2.1:8474"));
    }

    @Test void routingFailureIsRetained() throws Exception {
        var server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/proxies", exchange -> { exchange.sendResponseHeaders(503, -1); exchange.close(); });
        server.start();
        try {
            var router = new Main.Router("http://127.0.0.1:" + server.getAddress().getPort());
            assertThrows(Exception.class, () -> router.mapping(new InetSocketAddress("127.0.0.1", 8111)));
            assertThrows(IllegalStateException.class, () -> router.mapping(new InetSocketAddress("127.0.0.2", 8111)));
            assertEquals(1, router.routes.size());
        } finally { server.stop(0); }
    }

    @Test void typedKeysAreValidated() throws Exception {
        assertEquals("42", Main.key("{\"id\":{\"N\":\"42\"}}").get("id").n());
        assertThrows(IllegalArgumentException.class, () -> Main.key("{}"));
        assertThrows(IllegalArgumentException.class, () -> Main.key("{\"id\":{\"BOOL\":true}}"));
    }
}