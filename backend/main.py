from fastapi import FastAPI

app = FastAPI(title="OptionQuant AI Backend")

@app.get("/")
def read_root():
    return {"status": "online", "message": "OptionQuant AI Backend is running"}

@app.get("/health")
def health_check():
    return {"status": "ok"}
