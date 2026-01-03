package com.surveillance.video;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.web.socket.config.annotation.EnableWebSocket;
import org.springframework.web.socket.config.annotation.WebSocketConfigurer;
import org.springframework.web.socket.config.annotation.WebSocketHandlerRegistry;
import org.springframework.context.annotation.Bean;
import org.springframework.web.socket.WebSocketHandler;
import org.springframework.web.socket.handler.TextWebSocketHandler;
import org.springframework.web.socket.TextMessage;
import org.springframework.web.socket.WebSocketSession;
import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.kafka.annotation.EnableKafka;
import org.springframework.stereotype.Service;
import java.util.concurrent.CopyOnWriteArrayList;
import java.io.IOException;
import java.util.Base64;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

@SpringBootApplication
@EnableWebSocket
@EnableKafka
public class VideoServiceApplication implements WebSocketConfigurer {

    public static void main(String[] args) {
        SpringApplication.run(VideoServiceApplication.class, args);
    }

    private final VideoStreamService videoStreamService;

    public VideoServiceApplication(VideoStreamService videoStreamService) {
        this.videoStreamService = videoStreamService;
    }

    @Override
    public void registerWebSocketHandlers(WebSocketHandlerRegistry registry) {
        registry.addHandler(new VideoWebSocketHandler(videoStreamService), "/stream").setAllowedOrigins("*");
    }
}

@Service
class VideoStreamService {
    // Map to hold active sessions. 
    // In a real multi-tenant app, this would be Map<String, List<WebSocketSession>> where Key is CameraID/TenantID
    private final CopyOnWriteArrayList<WebSocketSession> sessions = new CopyOnWriteArrayList<>();

    public void addSession(WebSocketSession session) {
        sessions.add(session);
    }

    public void removeSession(WebSocketSession session) {
        sessions.remove(session);
    }

    // Kafka Listener that consumes frames
    @KafkaListener(topics = "camera-stream", groupId = "video-service-group")
    public void listen(byte[] frameData) {
        // Broadcast frame to all connected clients
        // In production, you'd inspect headers to route to specific tenants
        String base64Frame = Base64.getEncoder().encodeToString(frameData);
        TextMessage message = new TextMessage(base64Frame);

        for (WebSocketSession session : sessions) {
            try {
                if (session.isOpen()) {
                    session.sendMessage(message);
                }
            } catch (IOException e) {
                // Handle error (remove session, etc.)
            }
        }
    }
}

class VideoWebSocketHandler extends TextWebSocketHandler {
    
    private final VideoStreamService videoStreamService;

    public VideoWebSocketHandler(VideoStreamService videoStreamService) {
        this.videoStreamService = videoStreamService;
    }

    @Override
    public void afterConnectionEstablished(WebSocketSession session) throws Exception {
        videoStreamService.addSession(session);
    }

    @Override
    public void afterConnectionClosed(WebSocketSession session, org.springframework.web.socket.CloseStatus status) throws Exception {
        videoStreamService.removeSession(session);
    }
}
