// deej remix firmware - same serial protocol as vanilla deej ("512|1023|0|...\r\n"),
// with oversampling and smoothing so cheap pots stop jittering, and room for more pots.
// set the pins below, then flash. the deej app figures out the port and pot count by itself.

const int NUM_POTS = 5;
const int analogInputs[NUM_POTS] = {A0, A1, A2, A3, A4};

// must match baud_rate in the deej control panel
const long BAUD_RATE = 9600;

// how many raw reads are averaged per sample (more = steadier, slower)
const int OVERSAMPLE = 8;

// exponential smoothing, 0 < SMOOTHING <= 1. lower = smoother but laggier
const float SMOOTHING = 0.35;

// time between lines sent to the computer
const unsigned long SEND_INTERVAL_MS = 10;

float smoothedValues[NUM_POTS];
unsigned long lastSend = 0;

void setup() {
  for (int i = 0; i < NUM_POTS; i++) {
    pinMode(analogInputs[i], INPUT);
    smoothedValues[i] = readOversampled(analogInputs[i]);
  }

  Serial.begin(BAUD_RATE);
}

void loop() {
  for (int i = 0; i < NUM_POTS; i++) {
    float sample = readOversampled(analogInputs[i]);
    smoothedValues[i] += SMOOTHING * (sample - smoothedValues[i]);
  }

  unsigned long now = millis();
  if (now - lastSend >= SEND_INTERVAL_MS) {
    lastSend = now;
    sendValues();
  }
}

float readOversampled(int pin) {
  long total = 0;
  for (int i = 0; i < OVERSAMPLE; i++) {
    total += analogRead(pin);
  }

  return (float)total / OVERSAMPLE;
}

void sendValues() {
  // build the line with plain char buffers - String concatenation fragments the heap on small boards
  char line[NUM_POTS * 5 + 3];
  int pos = 0;

  for (int i = 0; i < NUM_POTS; i++) {
    int value = constrain((int)(smoothedValues[i] + 0.5), 0, 1023);
    pos += snprintf(line + pos, sizeof(line) - pos, i == 0 ? "%d" : "|%d", value);
  }

  Serial.println(line);
}
